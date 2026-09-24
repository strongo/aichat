package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
)

func sseWrite(w http.ResponseWriter, event, data string) {
	_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
	w.(http.Flusher).Flush()
}

func TestStream_BasicTextAndUsage(t *testing.T) {
	var gotBody messagesRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-ant" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != apiVersion {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_start", `{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":", world"}}`)
		sseWrite(w, "message_delta", `{"type":"message_delta","usage":{"output_tokens":7}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "sk-ant", Model: "claude-haiku-4-5-20251001"})
	if p.Name() != "anthropic" {
		t.Fatalf("Name() = %q", p.Name())
	}
	text, structured, usage, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
	}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "Hello, world" {
		t.Errorf("text = %q", text)
	}
	if structured != nil {
		t.Errorf("structured = %s, want nil", structured)
	}
	if usage == nil || usage.InputTokens != 20 || usage.OutputTokens != 7 || usage.CacheReadTokens != 5 || usage.CacheWriteTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
	if gotBody.MaxTokens != defaultMax {
		t.Errorf("MaxTokens = %d, want default %d", gotBody.MaxTokens, defaultMax)
	}
}

func TestStream_DefaultModelWhenUnset(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body messagesRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != defaultModel {
		t.Errorf("gotModel = %q, want %q", gotModel, defaultModel)
	}
}

func TestStream_SystemCacheControlOnLastStaticBlock(t *testing.T) {
	var gotBody messagesRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	req := ai.ChatRequest{
		System: "You are helpful.",
		Context: []ai.ContextBlock{
			{Scope: "s", Kind: ai.ContextStatic, Name: "skillA", Text: "static A"},
			{Scope: "s", Kind: ai.ContextStatic, Name: "skillB", Text: "static B"},
			{Scope: "s", Kind: ai.ContextDynamic, Name: "now", Text: "dyn now"},
		},
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	// system blocks: [system, staticA, staticB(cache)] -- dynamic must NOT
	// be in the cached system prefix at all (see M7: dynamic context must
	// not precede history).
	if len(gotBody.System) != 3 {
		t.Fatalf("System blocks = %+v", gotBody.System)
	}
	if !strings.Contains(gotBody.System[0].Text, "You are helpful.") {
		t.Errorf("System[0] = %+v", gotBody.System[0])
	}
	if gotBody.System[0].CacheControl != nil {
		t.Errorf("System[0].CacheControl must be nil (not the last static block)")
	}
	if !strings.Contains(gotBody.System[1].Text, "static A") || gotBody.System[1].CacheControl != nil {
		t.Errorf("System[1] = %+v", gotBody.System[1])
	}
	if !strings.Contains(gotBody.System[2].Text, "static B") {
		t.Errorf("System[2] = %+v", gotBody.System[2])
	}
	if gotBody.System[2].CacheControl == nil || gotBody.System[2].CacheControl.Type != "ephemeral" {
		t.Errorf("System[2].CacheControl = %+v, want ephemeral on the LAST static block", gotBody.System[2].CacheControl)
	}
	if len(gotBody.Messages) != 1 || len(gotBody.Messages[0].Content) != 1 {
		t.Fatalf("Messages = %+v", gotBody.Messages)
	}
	msgText := gotBody.Messages[0].Content[0].Text
	if !strings.Contains(msgText, "dyn now") {
		t.Errorf("dynamic context must be spliced into the last (current-turn) message: %q", msgText)
	}
	if !strings.HasSuffix(msgText, "hi") {
		t.Errorf("last message must still end with the original user text: %q", msgText)
	}
}

func TestStream_CacheControlOnLastHistoryMessage(t *testing.T) {
	var gotBody messagesRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	req := ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleUser, Text: "first"},
			{Role: ai.RoleAssistant, Text: "reply"},
			{Role: ai.RoleUser, Text: "second"},
		},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody.Messages) != 3 {
		t.Fatalf("Messages = %+v", gotBody.Messages)
	}
	// The last message BEFORE the final turn (index len-2, "reply") must
	// carry cache_control so the history itself is cached, not just system.
	histLast := gotBody.Messages[1].Content[0]
	if histLast.CacheControl == nil || histLast.CacheControl.Type != "ephemeral" {
		t.Errorf("Messages[1] (last history message) CacheControl = %+v, want ephemeral", histLast.CacheControl)
	}
	if gotBody.Messages[0].Content[0].CacheControl != nil {
		t.Errorf("Messages[0] must not carry cache_control")
	}
	if gotBody.Messages[2].Content[0].CacheControl != nil {
		t.Errorf("Messages[2] (current turn) must not carry cache_control")
	}
}

func TestStream_ResponseSchemaInstructsAndParses(t *testing.T) {
	var gotBody messagesRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+"```json\\n"+`"}}`)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"a\":1}\n"}}`)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+"```"+`"}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	req := ai.ChatRequest{ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	_, structured, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if string(structured) != `{"a":1}` {
		t.Errorf("structured = %s", structured)
	}
	last := gotBody.System[len(gotBody.System)-1]
	if !strings.Contains(last.Text, "JSON Schema") || !strings.Contains(last.Text, `"type":"object"`) {
		t.Errorf("last system block should instruct schema-only JSON reply: %q", last.Text)
	}
}

func TestStream_HTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status       int
		wantCode     string
		wantRetryVal bool
	}{
		{http.StatusUnauthorized, ai.ErrCodeAuth, false},
		{http.StatusForbidden, ai.ErrCodeAuth, false},
		{http.StatusTooManyRequests, ai.ErrCodeRateLimited, true},
		{http.StatusInternalServerError, ai.ErrCodeUpstream, true},
		{http.StatusBadRequest, ai.ErrCodeInvalid, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		}))
		p := New(Config{BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()})
		_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
		srv.Close()
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("status %d: err = %v, want *ai.Error", c.status, err)
		}
		if aiErr.Code != c.wantCode {
			t.Errorf("status %d: Code = %q, want %q", c.status, aiErr.Code, c.wantCode)
		}
		if aiErr.Retryable != c.wantRetryVal {
			t.Errorf("status %d: Retryable = %v, want %v", c.status, aiErr.Retryable, c.wantRetryVal)
		}
	}
}

func TestStream_StreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`)
		sseWrite(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Message != "overloaded" {
		t.Fatalf("err = %v", err)
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_RetriesOnlyBeforeFirstByte(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"try again"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "ok" {
		t.Errorf("text = %q", text)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestStream_TruncatedWithoutMessageStopIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`)
		// connection ends without message_stop.
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Fatalf("err = %v, want upstream truncation error", err)
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_MaxTokensFatalOnlyWithSchema(t *testing.T) {
	newSrv := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`)
			sseWrite(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}`)
			sseWrite(w, "message_stop", `{"type":"message_stop"}`)
		}))
	}

	srv1 := newSrv()
	defer srv1.Close()
	p1 := New(Config{BaseURL: srv1.URL, APIKey: "k"})
	_, _, _, err := ai.Collect(p1.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("without ResponseSchema, stop_reason=max_tokens must not be fatal: %v", err)
	}

	srv2 := newSrv()
	defer srv2.Close()
	p2 := New(Config{BaseURL: srv2.URL, APIKey: "k"})
	_, _, _, err = ai.Collect(p2.Stream(context.Background(), ai.ChatRequest{ResponseSchema: json.RawMessage(`{}`)}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) {
		t.Fatalf("with ResponseSchema, stop_reason=max_tokens must be fatal: %v", err)
	}
}

func TestStream_PingEventTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "ping", `{"type":"ping"}`)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "ok" {
		t.Errorf("text = %q", text)
	}
}

func TestMergeUsage_DoesNotClobberWithZeroFields(t *testing.T) {
	prev := &ai.Usage{InputTokens: 20, CacheReadTokens: 5, CacheWriteTokens: 2}
	next := &ai.Usage{OutputTokens: 7} // message_delta typically carries only this
	got := mergeUsage(prev, next)
	if got.InputTokens != 20 || got.CacheReadTokens != 5 || got.CacheWriteTokens != 2 {
		t.Errorf("mergeUsage clobbered earlier fields: %+v", got)
	}
	if got.OutputTokens != 7 {
		t.Errorf("mergeUsage did not take the newer field: %+v", got)
	}
}

func TestStream_CtxCancelMidBodyIsCanceled(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`)
		close(started)
		<-r.Context().Done()
		close(disconnected)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, _, err := ai.Collect(p.Stream(ctx, ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ErrCodeCanceled", err)
	}
	if aiErr.Retryable {
		t.Error("a cancellation must never be retryable")
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the client disconnect")
	}
}

func TestStream_RetryAfterHonouredOn429(t *testing.T) {
	attempts := 0
	var firstAttempt, secondAttempt time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			firstAttempt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		secondAttempt = time.Now()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if secondAttempt.Sub(firstAttempt) < 900*time.Millisecond {
		t.Errorf("retry happened after %v, want >= ~1s (Retry-After: 1)", secondAttempt.Sub(firstAttempt))
	}
}

func TestStream_429BillingErrorNotRetried(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"billing_error","message":"Your credit balance is too low to access the Anthropic API"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	start := time.Now()
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	elapsed := time.Since(start)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeQuota {
		t.Fatalf("err = %v, want ErrCodeQuota", err)
	}
	if aiErr.Retryable {
		t.Error("a billing-exhausted 429 must not be retryable")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (must not retry billing exhaustion)", attempts)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want fast (no Retry-After wait for a non-retryable error)", elapsed)
	}
}

func TestStream_NoRetryAfterWaitOnLastAttempt(t *testing.T) {
	// Mirrors ai/openaicompat's equivalent test: with the default
	// MaxAttempts=3, WaitOnRetryAfter is eligible to fire after attempts 1
	// and 2 ("not last") but MUST NOT fire after attempt 3 (the last) --
	// there's no attempt 4 to time a gap against, so this asserts on TOTAL
	// elapsed: two honoured ~2s waits plus small backoff+jitter lands well
	// under the headroom a bogus THIRD wait would add.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"try again later"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, APIKey: "k"})
	start := time.Now()
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (default MaxAttempts)", attempts)
	}
	if elapsed >= 5800*time.Millisecond {
		t.Errorf("elapsed = %v, want well under a 3rd Retry-After wait -- it must not be honoured on the last attempt", elapsed)
	}
	if elapsed < 3500*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~4s (Retry-After honoured on the two non-final attempts)", elapsed)
	}
}

func TestMessagesURL_TolerantOfTrailingV1(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com":     "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":    "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/v1":  "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/v1/": "https://api.anthropic.com/v1/messages",
	}
	for in, want := range cases {
		if got := messagesURL(in); got != want {
			t.Errorf("messagesURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStream_TrailingV1BaseURLDoesNotDoubleUp(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "k"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want %q (BaseURL's own trailing /v1/ must not double up)", gotPath, "/v1/messages")
	}
}

func TestNew_PanicsWithoutBaseURL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for missing BaseURL")
		}
	}()
	New(Config{})
}
