package openaicompat

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

func preparedTestRequest() ai.ChatRequest {
	return ai.ChatRequest{Model: "selected-model", MaxTokens: 17, System: "original system",
		Context:  []ai.ContextBlock{{Kind: ai.ContextStatic, Name: "rule", Text: "original static"}},
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "original question"}}}
}

func preparedTestProvider(rt http.RoundTripper, field OutputTokenField) *Provider {
	return New(Config{BaseURL: "https://example.invalid/v1", APIKey: "original-key", Model: "default-model",
		Headers: map[string]string{"X-Rule": "original"}, OutputTokenField: field,
		Guarded:    &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1},
		HTTPClient: &http.Client{Transport: rt}})
}

func preparedTestResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestPrepareGuardedFrozenWireAndInfo(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		configField OutputTokenField
	}{
		{"deepseek", "max_tokens", OutputTokenFieldMaxTokens},
		{"openai-default", "max_completion_tokens", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			var captured []byte
			rt := guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if r.Method != http.MethodPost || r.URL.String() != "https://example.invalid/v1/chat/completions" ||
					r.Header.Get("Authorization") != "Bearer original-key" || r.Header.Get("X-Rule") != "original" {
					t.Errorf("frozen request envelope: %s %s %v", r.Method, r.URL, r.Header)
				}
				var err error
				captured, err = io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				return preparedTestResponse(r, http.StatusOK, guardedSSE), nil
			})
			p := preparedTestProvider(rt, tc.configField)
			req := preparedTestRequest()
			prepared, err := p.PrepareGuarded(req)
			if err != nil || sends.Load() != 0 {
				t.Fatalf("prepare err=%v sends=%d", err, sends.Load())
			}
			info := prepared.Info()
			if info.WireRevision != 1 || info.Protocol != "chat-completions" || info.Model != "selected-model" ||
				info.MaxOutputTokens != 17 || string(info.OutputTokenField) != tc.field || info.BodySHA256 == ([sha256.Size]byte{}) {
				t.Fatalf("info=%+v", info)
			}
			// Mutation after preparation cannot alter the body, headers or client.
			req.Model = "changed-model"
			req.System = "changed system"
			req.Context[0].Text = "changed static"
			req.Messages[0].Text = "changed question"
			p.cfg.Headers["X-Rule"] = "changed"
			p.cfg.APIKey = "changed-key"
			p.cfg.BaseURL = "https://changed.invalid"
			p.cfg.HTTPClient.Transport = guardedRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("mutated client used")
				return nil, errors.New("mutated client")
			})
			text, _, _, err := ai.Collect(prepared.Stream(context.Background()))
			if err != nil || text != "ok" || sends.Load() != 1 {
				t.Fatalf("stream text=%q err=%v sends=%d", text, err, sends.Load())
			}
			if got := sha256.Sum256(captured); got != info.BodySHA256 {
				t.Fatalf("body digest mismatch: got %x want %x", got, info.BodySHA256)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(captured, &body); err != nil {
				t.Fatal(err)
			}
			if string(body[tc.field]) != "17" || string(body["model"]) != `"selected-model"` ||
				strings.Contains(string(captured), "changed") || !strings.Contains(string(captured), "original question") ||
				string(body["stream"]) != "true" {
				t.Fatalf("wrong frozen body: %s", captured)
			}
			other := "max_tokens"
			if tc.field == other {
				other = "max_completion_tokens"
			}
			if _, exists := body[other]; exists {
				t.Fatalf("both token fields: %s", captured)
			}
		})
	}
}

func TestPreparedChatCopyAndIteratorShareOneLease(t *testing.T) {
	var sends atomic.Int32
	p := preparedTestProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		time.Sleep(time.Millisecond)
		return preparedTestResponse(r, http.StatusOK, guardedSSE), nil
	}), OutputTokenFieldMaxTokens)
	prepared, err := p.PrepareGuarded(preparedTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	copyOfPrepared := *prepared
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, r := range []*PreparedChat{prepared, &copyOfPrepared} {
		wg.Add(1)
		go func(r *PreparedChat) {
			defer wg.Done()
			<-start
			_, _, _, err := ai.Collect(r.Stream(context.Background()))
			results <- err
		}(r)
	}
	close(start)
	wg.Wait()
	close(results)
	successes, refusals := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			var aiErr *ai.Error
			if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
				t.Fatalf("loser error=%v", err)
			}
			refusals++
		}
	}
	if sends.Load() != 1 || successes != 1 || refusals != 1 {
		t.Fatalf("sends=%d success=%d refused=%d", sends.Load(), successes, refusals)
	}
	iterator := prepared.Stream(context.Background())
	for i := 0; i < 2; i++ {
		_, _, _, err := ai.Collect(iterator)
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid || sends.Load() != 1 {
			t.Fatalf("repeat %d err=%v sends=%d", i, err, sends.Load())
		}
	}
	var zero PreparedChat
	if zero.Info() != (PreparedChatInfo{}) {
		t.Fatal("zero info not empty")
	}
	for _, r := range []*PreparedChat{&zero, nil} {
		_, _, _, err := ai.Collect(r.Stream(context.Background()))
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid || sends.Load() != 1 || r.Info() != (PreparedChatInfo{}) {
			t.Fatalf("zero/nil err=%v sends=%d", err, sends.Load())
		}
	}
}

func TestPreparedChatFailureIsOneAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
	}{
		{"rate limit", http.StatusTooManyRequests, nil},
		{"server error", http.StatusServiceUnavailable, nil},
		{"reasoning named", http.StatusBadRequest, nil},
		{"redirect temporary", http.StatusTemporaryRedirect, nil},
		{"redirect permanent", http.StatusPermanentRedirect, nil},
		{"transport timeout", 0, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			p := preparedTestProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if tc.err != nil {
					return nil, tc.err
				}
				body := `{"error":{"message":"reasoning_effort unsupported"}}`
				resp := preparedTestResponse(r, tc.status, body)
				if tc.status == http.StatusTemporaryRedirect || tc.status == http.StatusPermanentRedirect {
					resp.Header.Set("Location", "https://other.invalid/chat/completions")
				}
				return resp, nil
			}), OutputTokenFieldMaxTokens)
			prepared, err := p.PrepareGuarded(preparedTestRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := ai.Collect(prepared.Stream(context.Background())); err == nil || sends.Load() != 1 {
				t.Fatalf("err=%v sends=%d", err, sends.Load())
			}
		})
	}
}

func TestPrepareGuardedRejectsUnsupportedBeforeSend(t *testing.T) {
	var sends atomic.Int32
	rt := guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedTestResponse(r, http.StatusOK, guardedSSE), nil
	})
	for _, tc := range []struct {
		name string
		make func() (*Provider, ai.ChatRequest)
	}{
		{"nil provider", func() (*Provider, ai.ChatRequest) { return nil, preparedTestRequest() }},
		{"no guard", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.cfg.Guarded = nil
			return p, preparedTestRequest()
		}},
		{"no client", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.cfg.HTTPClient = nil
			return p, preparedTestRequest()
		}},
		{"bad attempts", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.cfg.Guarded.MaxOutboundAttempts = 2
			return p, preparedTestRequest()
		}},
		{"bad guard cap", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.cfg.Guarded.MaxOutputTokens = 0
			return p, preparedTestRequest()
		}},
		{"no request cap", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.MaxTokens = 0
			return preparedTestProvider(rt, ""), r
		}},
		{"over cap", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.MaxTokens = 18
			return preparedTestProvider(rt, ""), r
		}},
		{"empty model", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Model = ""
			return preparedTestProvider(rt, ""), r
		}},
		{"auto model", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Model = ai.ModelAuto
			return preparedTestProvider(rt, ""), r
		}},
		{"blank model", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Model = " "
			return preparedTestProvider(rt, ""), r
		}},
		{"bad output field", func() (*Provider, ai.ChatRequest) { return preparedTestProvider(rt, "bad"), preparedTestRequest() }},
		{"tools", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Tools = []ai.Tool{{Name: "run"}}
			return preparedTestProvider(rt, ""), r
		}},
		{"tool choice", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.ToolChoice = ai.ToolChoiceRequired
			return preparedTestProvider(rt, ""), r
		}},
		{"schema", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.ResponseSchema = json.RawMessage(`{}`)
			return preparedTestProvider(rt, ""), r
		}},
		{"strict schema", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			yes := true
			r.StrictSchema = &yes
			return preparedTestProvider(rt, ""), r
		}},
		{"reasoning", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Reasoning = ai.ReasoningHigh
			return preparedTestProvider(rt, ""), r
		}},
		{"tool role", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Messages[0].Role = ai.RoleTool
			return preparedTestProvider(rt, ""), r
		}},
		{"tool calls", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Messages[0].ToolCalls = []ai.ToolCall{{Name: "run"}}
			return preparedTestProvider(rt, ""), r
		}},
		{"tool results", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Messages[0].ToolResults = []ai.ToolResult{{CallID: "x"}}
			return preparedTestProvider(rt, ""), r
		}},
		{"provider state", func() (*Provider, ai.ChatRequest) {
			r := preparedTestRequest()
			r.Messages[0].ProviderState = json.RawMessage(`{}`)
			return preparedTestProvider(rt, ""), r
		}},
		{"invalid endpoint", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.cfg.BaseURL = "ftp://example.invalid"
			return p, preparedTestRequest()
		}},
		{"marshal failure", func() (*Provider, ai.ChatRequest) {
			p := preparedTestProvider(rt, "")
			p.marshalPrepared = func(chatRequestBody) ([]byte, error) { return nil, errors.New("encode failed") }
			return p, preparedTestRequest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, req := tc.make()
			prepared, err := p.PrepareGuarded(req)
			var aiErr *ai.Error
			if prepared != nil || !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid || sends.Load() != 0 {
				t.Fatalf("prepared=%v err=%v sends=%d", prepared, err, sends.Load())
			}
		})
	}
}

func TestPreparedChatCorruptEndpointRefusesWithoutSend(t *testing.T) {
	var sends atomic.Int32
	p := preparedTestProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedTestResponse(r, http.StatusOK, guardedSSE), nil
	}), OutputTokenFieldMaxTokens)
	prepared, err := p.PrepareGuarded(preparedTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	// A damaged private request envelope must fail before transport.
	prepared.state.endpoint = "http://[::1"
	if _, _, _, err := ai.Collect(prepared.Stream(context.Background())); err == nil || sends.Load() != 0 {
		t.Fatalf("damaged envelope err=%v sends=%d", err, sends.Load())
	}
}

func TestPreparedChatCanceledOrTruncatedDoesNotResend(t *testing.T) {
	var sends atomic.Int32
	p := preparedTestProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return preparedTestResponse(r, http.StatusOK, "data: {\"choices\":[]}\n\n"), nil
	}), OutputTokenFieldMaxTokens)
	prepared, err := p.PrepareGuarded(preparedTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := ai.Collect(prepared.Stream(ctx)); err == nil || sends.Load() != 0 {
		t.Fatalf("canceled err=%v sends=%d", err, sends.Load())
	}
	if _, _, _, err := ai.Collect(prepared.Stream(context.Background())); err == nil || sends.Load() != 0 {
		t.Fatalf("canceled object retried: err=%v sends=%d", err, sends.Load())
	}
	truncated, err := p.PrepareGuarded(preparedTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ai.Collect(truncated.Stream(context.Background())); err == nil || sends.Load() != 1 {
		t.Fatalf("truncated err=%v sends=%d", err, sends.Load())
	}
	if _, _, _, err := ai.Collect(truncated.Stream(context.Background())); err == nil || sends.Load() != 1 {
		t.Fatalf("truncated object retried: err=%v sends=%d", err, sends.Load())
	}
}

func TestPreparedChatCanceledDuringTransportDoesNotResend(t *testing.T) {
	var sends atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	p := preparedTestProvider(guardedRoundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		cancel()
		return nil, context.Canceled
	}), OutputTokenFieldMaxTokens)
	prepared, err := p.PrepareGuarded(preparedTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = ai.Collect(prepared.Stream(ctx))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled || sends.Load() != 1 {
		t.Fatalf("cancel err=%v sends=%d", err, sends.Load())
	}
	if _, _, _, err := ai.Collect(prepared.Stream(context.Background())); err == nil || sends.Load() != 1 {
		t.Fatalf("cancel retried err=%v sends=%d", err, sends.Load())
	}
}
