package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
)

const preparedSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func preparedProvider(rt http.RoundTripper) (*Provider, Config) {
	cfg := Config{BaseURL: "https://example.invalid/v1", APIKey: "initial-key",
		Headers: map[string]string{}, HTTPClient: &http.Client{Transport: rt},
		Guarded: &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1,
			TextModels: map[string]bool{guardedTextModelHaiku45: true}}}
	return New(cfg), cfg
}

func preparedRequest() ai.ChatRequest {
	return ai.ChatRequest{Model: guardedTextModelHaiku45, MaxTokens: 17, System: "original system",
		Context: []ai.ContextBlock{
			{Kind: ai.ContextStatic, Name: "policy", Text: "original stable"},
			{Kind: ai.ContextDynamic, Name: "now", Text: "original dynamic"},
		},
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "original question"},
			{Role: ai.RoleAssistant, Text: "original answer"},
			{Role: ai.RoleUser, Text: "original followup"}}}
}

func preparedResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{Request: req, StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body))}
}

func TestPreparedFrozenWireAndSharedInfo(t *testing.T) {
	var sends atomic.Int32
	var captured []byte
	provider, cfg := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		if req.Method != http.MethodPost || req.URL.String() != "https://example.invalid/v1/messages" ||
			req.Header.Get("x-api-key") != "initial-key" || req.Header.Get("anthropic-version") != apiVersion ||
			req.Header.Get("Accept") != "text/event-stream" || req.Header.Get("anthropic-beta") != "" {
			t.Errorf("changed guarded envelope: %s %s %v", req.Method, req.URL, req.Header)
		}
		var err error
		captured, err = io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	req := preparedRequest()
	var generic ai.GuardedChatPreparer = provider
	call, err := generic.PrepareGuardedChat(req)
	if err != nil || sends.Load() != 0 {
		t.Fatalf("prepare err=%v sends=%d", err, sends.Load())
	}
	info := call.PreparedInfo()
	if info.WireRevision != 1 || info.Protocol != "anthropic-messages" ||
		info.Model != guardedTextModelHaiku45 || info.OutputTokenField != "max_tokens" ||
		info.MaxOutputTokens != 17 || info.BodySHA256 == ([sha256.Size]byte{}) {
		t.Fatalf("info=%+v", info)
	}
	req.Model = "changed"
	req.System = "changed"
	req.Context[0].Text = "changed"
	req.Messages[0].Text = "changed"
	cfg.Headers["anthropic-beta"] = "unpriced-feature"
	cfg.Guarded.TextModels[guardedTextModelHaiku45] = false
	cfg.Guarded.MaxOutputTokens = 1
	cfg.HTTPClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("mutated transport used")
		return nil, errors.New("mutated transport")
	})
	provider.cfg.BaseURL = "https://changed.invalid"
	provider.cfg.APIKey = "changed-key"
	text, structured, usage, err := ai.Collect(call.Stream(context.Background()))
	if err != nil || text != "ok" || structured != nil || usage == nil ||
		usage.InputTokens != 3 || usage.OutputTokens != 2 || sends.Load() != 1 {
		t.Fatalf("text=%q structured=%s usage=%+v err=%v sends=%d", text, structured, usage, err, sends.Load())
	}
	if got := sha256.Sum256(captured); got != info.BodySHA256 {
		t.Fatalf("body digest %x != %x", got, info.BodySHA256)
	}
	if strings.Contains(string(captured), "changed") || strings.Contains(string(captured), "cache_control") ||
		strings.Contains(string(captured), "thinking") || strings.Contains(string(captured), "tools") ||
		!strings.Contains(string(captured), "original followup") {
		t.Fatalf("wrong frozen text-only body: %s", captured)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(captured, &body); err != nil || string(body["model"]) != `"`+guardedTextModelHaiku45+`"` ||
		string(body["max_tokens"]) != "17" || string(body["stream"]) != "true" {
		t.Fatalf("bad final JSON %s: %v", captured, err)
	}
}

func TestPreparedPolicyAndModelSnapshot(t *testing.T) {
	provider, cfg := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	delete(cfg.Guarded.TextModels, guardedTextModelHaiku45)
	cfg.Guarded.TextModels["claude-haiku-99"] = true
	cfg.Guarded.MaxOutboundAttempts = 7
	cfg.Guarded.MaxOutputTokens = 1
	if _, err := provider.PrepareGuarded(preparedRequest()); err != nil {
		t.Fatalf("caller mutation changed copied policy: %v", err)
	}
	future := preparedRequest()
	future.Model = "claude-haiku-99"
	if _, err := provider.PrepareGuarded(future); err == nil {
		t.Fatal("future Haiku admitted by family heuristic")
	}
	// Even an operator entry cannot expand the adapter's exact supported wire
	// subset to an unknown or adaptive model.
	provider.cfg.Guarded.TextModels[future.Model] = true
	if _, err := provider.PrepareGuarded(future); err == nil {
		t.Fatal("unknown model admitted by operator allowlist alone")
	}
	adaptive := preparedRequest()
	adaptive.Model = "claude-sonnet-4-6"
	provider.cfg.Guarded.TextModels[adaptive.Model] = true
	if _, err := provider.PrepareGuarded(adaptive); err == nil {
		t.Fatal("adaptive thinking model admitted")
	}
}

func TestPreparedConstructorSnapshotsNonemptyHeadersAndRefusesThem(t *testing.T) {
	provider, cfg := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	// A nonempty constructor map is copied; deleting from the caller's map
	// cannot make an unreviewed beta header disappear from guarded policy.
	cfg.Headers["anthropic-beta"] = "feature"
	withHeader := New(cfg)
	delete(cfg.Headers, "anthropic-beta")
	if _, err := withHeader.PrepareGuarded(preparedRequest()); err == nil {
		t.Fatal("constructor header mutation opened guarded preparation")
	}
	if _, err := provider.PrepareGuarded(preparedRequest()); err != nil {
		t.Fatalf("original header-free provider changed: %v", err)
	}
}

func TestPreparedMarshalAndRequestFaultsRefuse(t *testing.T) {
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	provider.marshalPrepared = func(messagesRequestBody) ([]byte, error) {
		return nil, errors.New("synthetic marshal failure")
	}
	if _, err := provider.PrepareGuarded(preparedRequest()); err == nil {
		t.Fatal("marshal failure admitted")
	}
	provider.marshalPrepared = func(body messagesRequestBody) ([]byte, error) { return json.Marshal(body) }
	prepared, err := provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	prepared.state.endpoint = "\x00"
	if _, err := prepared.state.send(context.Background()); err == nil {
		t.Fatal("invalid frozen request endpoint accepted")
	}
}

func TestPreparedRejectsUnpricedShapeBeforeSend(t *testing.T) {
	var sends atomic.Int32
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	for _, tc := range []struct {
		name   string
		mutate func(*ai.ChatRequest)
	}{
		{"empty model", func(r *ai.ChatRequest) { r.Model = "" }},
		{"auto model", func(r *ai.ChatRequest) { r.Model = ai.ModelAuto }},
		{"space model", func(r *ai.ChatRequest) { r.Model = " " + guardedTextModelHaiku45 }},
		{"zero cap", func(r *ai.ChatRequest) { r.MaxTokens = 0 }},
		{"over cap", func(r *ai.ChatRequest) { r.MaxTokens = 18 }},
		{"tool", func(r *ai.ChatRequest) { r.Tools = []ai.Tool{{Name: "call"}} }},
		{"tool choice", func(r *ai.ChatRequest) { r.ToolChoice = ai.ToolChoiceAuto }},
		{"schema", func(r *ai.ChatRequest) { r.ResponseSchema = json.RawMessage(`{}`) }},
		{"strict", func(r *ai.ChatRequest) { v := false; r.StrictSchema = &v }},
		{"reasoning", func(r *ai.ChatRequest) { r.Reasoning = ai.ReasoningLow }},
		{"no messages", func(r *ai.ChatRequest) { r.Messages = nil }},
		{"role", func(r *ai.ChatRequest) { r.Messages[0].Role = ai.RoleTool }},
		{"empty text", func(r *ai.ChatRequest) { r.Messages[0].Text = " " }},
		{"tool calls", func(r *ai.ChatRequest) { r.Messages[0].ToolCalls = []ai.ToolCall{{ID: "call"}} }},
		{"tool results", func(r *ai.ChatRequest) { r.Messages[0].ToolResults = []ai.ToolResult{{CallID: "call"}} }},
		{"provider state", func(r *ai.ChatRequest) { r.Messages[0].ProviderState = json.RawMessage(`{}`) }},
		{"context", func(r *ai.ChatRequest) { r.Context[0].Kind = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := preparedRequest()
			tc.mutate(&req)
			if _, err := provider.PrepareGuarded(req); err == nil || sends.Load() != 0 {
				t.Fatalf("unsupported request err=%v sends=%d", err, sends.Load())
			}
		})
	}
	for _, mutate := range []func(){
		func() { provider.cfg.Guarded.MaxOutboundAttempts = 2 },
		func() { provider.cfg.Guarded.MaxOutboundAttempts = 1; provider.cfg.Guarded.MaxOutputTokens = 0 },
		func() {
			provider.cfg.Guarded.MaxOutputTokens = 17
			provider.cfg.Guarded.TextModels[guardedTextModelHaiku45] = false
		},
		func() {
			provider.cfg.Guarded.TextModels[guardedTextModelHaiku45] = true
			provider.cfg.Headers["anthropic-beta"] = "feature"
		},
		func() {
			delete(provider.cfg.Headers, "anthropic-beta")
			provider.cfg.BaseURL = "https://example.invalid/path?key=value"
		},
	} {
		mutate()
		if _, err := provider.PrepareGuarded(preparedRequest()); err == nil || sends.Load() != 0 {
			t.Fatalf("invalid provider config err=%v sends=%d", err, sends.Load())
		}
	}
	provider.cfg.BaseURL = "https://example.invalid"
	provider.cfg.Guarded = nil
	if _, err := provider.PrepareGuarded(preparedRequest()); err == nil {
		t.Fatal("unguarded provider prepared call")
	}
	var nilProvider *Provider
	if _, err := nilProvider.PrepareGuarded(preparedRequest()); err == nil {
		t.Fatal("nil provider prepared call")
	}
}

func TestPreparedSharedLeaseAndZeroValues(t *testing.T) {
	var sends atomic.Int32
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		time.Sleep(time.Millisecond)
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	prepared, err := provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	copyOfPrepared := *prepared
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, candidate := range []*PreparedChat{prepared, &copyOfPrepared} {
		wg.Add(1)
		go func(r *PreparedChat) {
			defer wg.Done()
			<-start
			_, _, _, err := ai.Collect(r.Stream(context.Background()))
			results <- err
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(results)
	success, refused := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			var ae *ai.Error
			if !errors.As(err, &ae) || ae.Code != ai.ErrCodeInvalid {
				t.Fatalf("unexpected loser error: %v", err)
			}
			refused++
		}
	}
	if success != 1 || refused != 1 || sends.Load() != 1 {
		t.Fatalf("success=%d refused=%d sends=%d", success, refused, sends.Load())
	}
	iterator := prepared.Stream(context.Background())
	for i := 0; i < 2; i++ {
		_, _, _, err := ai.Collect(iterator)
		if err == nil || sends.Load() != 1 {
			t.Fatalf("repeat err=%v sends=%d", err, sends.Load())
		}
	}
	var zero PreparedChat
	for _, candidate := range []*PreparedChat{&zero, nil} {
		if candidate.Info() != (PreparedChatInfo{}) || candidate.PreparedInfo() != (ai.PreparedChatInfo{}) {
			t.Fatal("zero/nil info not empty")
		}
		_, _, _, err := ai.Collect(candidate.Stream(context.Background()))
		if err == nil || sends.Load() != 1 {
			t.Fatalf("zero/nil err=%v sends=%d", err, sends.Load())
		}
	}
}

func TestPreparedFailuresNeverResend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"429", http.StatusTooManyRequests, `{"error":{"message":"slow"}}`, nil},
		{"503", http.StatusServiceUnavailable, `{"error":{"message":"down"}}`, nil},
		{"307", http.StatusTemporaryRedirect, "", nil},
		{"308", http.StatusPermanentRedirect, "", nil},
		{"timeout", 0, "", context.DeadlineExceeded},
		{"truncated", http.StatusOK, "event: message_start\ndata: {}\n\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				if tc.err != nil {
					return nil, tc.err
				}
				resp := preparedResponse(req, tc.status, tc.body)
				switch tc.status {
				case http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
					resp.Header.Set("Location", "https://redirect.invalid/v1/messages")
				case http.StatusTooManyRequests:
					resp.Header.Set("Retry-After", "60")
				}
				return resp, nil
			}))
			prepared, err := provider.PrepareGuarded(preparedRequest())
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = ai.Collect(prepared.Stream(context.Background()))
			if err == nil || sends.Load() != 1 {
				t.Fatalf("err=%v sends=%d", err, sends.Load())
			}
			_, _, _, err = ai.Collect(prepared.Stream(context.Background()))
			if err == nil || sends.Load() != 1 {
				t.Fatalf("replay err=%v sends=%d", err, sends.Load())
			}
		})
	}
	var sends atomic.Int32
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	prepared, err := provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = ai.Collect(prepared.Stream(ctx))
	if err == nil || sends.Load() != 0 {
		t.Fatalf("canceled before send err=%v sends=%d", err, sends.Load())
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	provider, _ = preparedProvider(roundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		cancel()
		return nil, errors.New("canceled during transport")
	}))
	prepared, err = provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = ai.Collect(prepared.Stream(ctx))
	var ae *ai.Error
	if !errors.As(err, &ae) || ae.Code != ai.ErrCodeCanceled || sends.Load() != 1 {
		t.Fatalf("in-transport cancellation err=%v sends=%d", err, sends.Load())
	}
}

func TestPreparedRequiresObservedTerminalUsage(t *testing.T) {
	for _, tc := range []struct {
		name, start, deltas string
		complete            bool
		output              int64
	}{
		{"complete", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, true, 2},
		{"explicit zero output", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":0}}`, true, 0},
		{"terminal zero overrides initial count", `{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":9}}}`,
			`{"type":"message_delta","usage":{"output_tokens":0}}`, true, 0},
		{"no start", "", `{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"delta before start", `{"type":"message_delta","usage":{"output_tokens":2}}` + "\n\nevent: message_start\ndata: " +
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"missing initial usage", `{"type":"message_start","message":{}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"missing initial message", `{"type":"message_start"}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"empty initial usage", `{"type":"message_start","message":{"usage":{}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"negative input", `{"type":"message_start","message":{"usage":{"input_tokens":-1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"negative initial output", `{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":-1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"negative cache read", `{"type":"message_start","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":-1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"negative cache write", `{"type":"message_start","message":{"usage":{"input_tokens":3,"cache_creation_input_tokens":-1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"no message delta", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			"", false, 0},
		{"early stop then late usage", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_stop"}` + "\n\nevent: message_delta\ndata: " +
				`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"duplicate start", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`, false, 0},
		{"accounting after stop", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}` + "\n\nevent: message_stop\ndata: " +
				`{"type":"message_stop"}` + "\n\nevent: message_delta\ndata: " +
				`{"type":"message_delta","usage":{"output_tokens":4}}`, false, 0},
		{"delta without usage", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta"}`, false, 0},
		{"empty terminal usage", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{}}`, false, 0},
		{"terminal missing output", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"input_tokens":3}}`, false, 0},
		{"negative output", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":-1}}`, false, 0},
		{"negative delta input", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"input_tokens":-1,"output_tokens":2}}`, false, 0},
		{"negative terminal cache write", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2,"cache_creation_input_tokens":-1}}`, false, 0},
		{"later empty delta", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","usage":{}}`, false, 0},
		{"malformed initial count", `{"type":"message_start","message":{"usage":{"input_tokens":"3"}}}`,
			`{"type":"message_delta","usage":{"output_tokens":2}}`, false, 0},
		{"malformed terminal count", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"message_delta","usage":{"output_tokens":"2"}}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			stream := ""
			if tc.start != "" {
				stream += "event: message_start\ndata: " + tc.start + "\n\n"
			}
			if tc.deltas != "" {
				stream += "event: message_delta\ndata: " + tc.deltas + "\n\n"
			}
			stream += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				return preparedResponse(req, http.StatusOK, stream), nil
			}))
			prepared, err := provider.PrepareGuarded(preparedRequest())
			if err != nil {
				t.Fatal(err)
			}
			completed, usageEvents := 0, 0
			var finalErr error
			var finalUsage *ai.Usage
			for event, eventErr := range prepared.Stream(context.Background()) {
				if event.Type == ai.EventCompleted {
					completed++
					finalUsage = event.Usage
				}
				if event.Type == ai.EventUsage {
					usageEvents++
				}
				if eventErr != nil {
					finalErr = eventErr
				}
			}
			if sends.Load() != 1 {
				t.Fatalf("sends=%d", sends.Load())
			}
			if tc.complete {
				if finalErr != nil || completed != 1 || usageEvents != 1 || finalUsage == nil ||
					finalUsage.InputTokens != 3 || finalUsage.OutputTokens != tc.output {
					t.Fatalf("complete err=%v completed=%d usageEvents=%d usage=%+v", finalErr, completed, usageEvents, finalUsage)
				}
			} else if finalErr == nil || completed != 0 || usageEvents != 0 {
				t.Fatalf("incomplete err=%v completed=%d usageEvents=%d", finalErr, completed, usageEvents)
			}
		})
	}
}

func TestPreparedTerminalPresentZerosReplaceEarlierCounts(t *testing.T) {
	stream := "event: message_start\ndata: " +
		`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":4}}}` +
		"\n\nevent: message_delta\ndata: " +
		`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}` +
		"\n\nevent: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	var sends atomic.Int32
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedResponse(req, http.StatusOK, stream), nil
	}))
	prepared, err := provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, _, usage, err := ai.Collect(prepared.Stream(context.Background()))
	if err != nil || sends.Load() != 1 || usage == nil ||
		usage.InputTokens != 0 || usage.OutputTokens != 0 ||
		usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 {
		t.Fatalf("final zero usage=%+v err=%v sends=%d", usage, err, sends.Load())
	}
}

func TestPreparedConsumerStopsAtVerifiedUsage(t *testing.T) {
	var sends atomic.Int32
	provider, _ := preparedProvider(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedResponse(req, http.StatusOK, preparedSSE), nil
	}))
	prepared, err := provider.PrepareGuarded(preparedRequest())
	if err != nil {
		t.Fatal(err)
	}
	sawUsage, sawCompletion := false, false
	for event, eventErr := range prepared.Stream(context.Background()) {
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		if event.Type == ai.EventCompleted {
			sawCompletion = true
		}
		if event.Type == ai.EventUsage {
			sawUsage = true
			break
		}
	}
	if !sawUsage || sawCompletion || sends.Load() != 1 {
		t.Fatalf("usage=%v completion=%v sends=%d", sawUsage, sawCompletion, sends.Load())
	}
}
