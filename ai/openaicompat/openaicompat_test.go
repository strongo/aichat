package openaicompat

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

func sseWrite(w http.ResponseWriter, data string) {
	_, _ = io.WriteString(w, "data: "+data+"\n\n")
	w.(http.Flusher).Flush()
}

func TestStream_BasicTextAndUsage(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{"content":"Hello"}}]}`)
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{"content":", world"}}]}`)
		sseWrite(w, `{"model":"gpt-5","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-5"})
	if p.Name() != "openai-compatible" {
		t.Fatalf("Name() = %q", p.Name())
	}

	req := ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}
	text, structured, usage, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "Hello, world" {
		t.Errorf("text = %q", text)
	}
	if structured != nil {
		t.Errorf("structured = %s, want nil", structured)
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.CacheReadTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
	if gotBody.Model != "gpt-5" || !gotBody.Stream || gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Errorf("gotBody = %+v", gotBody)
	}
}

func TestStream_ModelAutoUsesConfigDefault(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "default-model"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Model: ai.ModelAuto}))
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "default-model" {
		t.Errorf("gotModel = %q", gotModel)
	}

	_, _, _, err = ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Model: ""}))
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "default-model" {
		t.Errorf("gotModel (empty) = %q", gotModel)
	}
}

func TestStream_SystemHoldsOnlyStaticContext(t *testing.T) {
	var gotMessages []chatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotMessages = body.Messages
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		System: "You are helpful.",
		Context: []ai.ContextBlock{
			{Scope: "s", Kind: ai.ContextDynamic, Name: "now", Text: "now=2026"},
			{Scope: "s", Kind: ai.ContextStatic, Name: "calendar_skill", Text: "static instructions"},
		},
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotMessages) != 2 {
		t.Fatalf("gotMessages = %+v", gotMessages)
	}
	sysMsg := gotMessages[0]
	if sysMsg.Role != "system" {
		t.Fatalf("first message role = %q", sysMsg.Role)
	}
	if !strings.Contains(sysMsg.Content, "static instructions") {
		t.Errorf("system message missing static context: %q", sysMsg.Content)
	}
	if strings.Contains(sysMsg.Content, "now=2026") {
		t.Errorf("dynamic context must NOT be in the cached system message: %q", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "You are helpful.") {
		t.Errorf("system prompt missing: %q", sysMsg.Content)
	}
	last := gotMessages[1]
	if last.Role != "user" {
		t.Fatalf("gotMessages[1].Role = %q", last.Role)
	}
	if !strings.Contains(last.Content, "now=2026") {
		t.Errorf("dynamic context must prefix the last (current-turn) message: %q", last.Content)
	}
	if !strings.HasSuffix(last.Content, "hi") {
		t.Errorf("last message must still end with the original user text: %q", last.Content)
	}
	iContext := strings.Index(last.Content, "[context]")
	iText := strings.Index(last.Content, "hi")
	if iContext < 0 || iContext > iText {
		t.Errorf("dynamic context must be a clearly delimited PREFIX of the last message: %q", last.Content)
	}
}

func TestStream_ResponseSchemaProducesStructured(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"{\"a\":"}}]}`)
		sseWrite(w, `{"choices":[{"delta":{"content":"1}"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: "give me json"}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	_, structured, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if string(structured) != `{"a":1}` {
		t.Errorf("structured = %s", structured)
	}
	if gotBody.ResponseFormat == nil || gotBody.ResponseFormat.Type != "json_schema" || !gotBody.ResponseFormat.JSONSchema.Strict {
		t.Errorf("ResponseFormat = %+v", gotBody.ResponseFormat)
	}
}

func TestStream_StrictSchemaOptOut(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	no := false
	req := ai.ChatRequest{ResponseSchema: json.RawMessage(`{"type":"object"}`), StrictSchema: &no}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.ResponseFormat.JSONSchema.Strict {
		t.Error("StrictSchema=false must turn strict mode off")
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
		p := New(Config{BaseURL: srv.URL, Model: "m", HTTPClient: srv.Client()})
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
		sseWrite(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
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

func TestStream_HeadersForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Custom")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m", Headers: map[string]string{"X-Custom": "v1"}})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1" {
		t.Errorf("X-Custom = %q", got)
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

func TestStream_UsesMaxCompletionTokens(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{MaxTokens: 512}))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.MaxCompletionTokens != 512 {
		t.Errorf("MaxCompletionTokens = %d, want 512", gotBody.MaxCompletionTokens)
	}
}

func TestStream_TruncatedWithoutDoneIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		// connection just ends: no [DONE] marker.
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Fatalf("err = %v, want upstream truncation error", err)
	}
	if text != "partial" {
		t.Errorf("text = %q, want partial text kept before truncation", text)
	}
}

func TestStream_MidStreamErrorChunkIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		sseWrite(w, `{"error":{"message":"content filtered","type":"content_policy"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Message != "content filtered" {
		t.Fatalf("err = %v", err)
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_FinishReasonLengthFatalOnlyWithSchema(t *testing.T) {
	newSrv := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			sseWrite(w, `{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`)
			sseWrite(w, "[DONE]")
		}))
	}

	srv1 := newSrv()
	defer srv1.Close()
	p1 := New(Config{BaseURL: srv1.URL, Model: "m"})
	_, _, _, err := ai.Collect(p1.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("without ResponseSchema, finish_reason=length must not be fatal: %v", err)
	}

	srv2 := newSrv()
	defer srv2.Close()
	p2 := New(Config{BaseURL: srv2.URL, Model: "m"})
	_, _, _, err = ai.Collect(p2.Stream(context.Background(), ai.ChatRequest{ResponseSchema: json.RawMessage(`{}`)}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) {
		t.Fatalf("with ResponseSchema, finish_reason=length must be fatal: %v", err)
	}
}

func TestStream_BadChunkIsFatalPair(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `not json at all`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var lastEvent ai.Event
	var lastErr error
	n := 0
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{}) {
		n++
		lastEvent, lastErr = ev, err
		if err != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected a fatal error")
	}
	if lastEvent.Type != ai.EventError || lastEvent.Error == nil {
		t.Fatalf("fatal yield must carry Event{Type: EventError, Error: e}, got %+v", lastEvent)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 yields (EventStarted, fatal), got %d", n)
	}
}

func TestStream_CtxCancelMidBodyIsCanceled(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		close(started)
		<-r.Context().Done() // the server observes the client disconnect
		close(block)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
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
	case <-block:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the client disconnect")
	}
}

func TestStream_NoRetryOnceStreamingHasStarted(t *testing.T) {
	// The retry helper only wraps the initial request (see
	// ai/internal/retry's doc and Provider.Stream's use of it): once the
	// server has responded 200 and events have started flowing, a
	// mid-stream failure -- even a truncation, which looks superficially
	// like the kind of transient condition retryable errors model -- must
	// never trigger a second HTTP request.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		// connection ends without [DONE]: a fatal truncation, not a retry trigger.
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err == nil {
		t.Fatal("expected a truncation error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (no retry after the first byte)", attempts)
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
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
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

func TestStream_429QuotaExhaustedNotRetried(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	start := time.Now()
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	elapsed := time.Since(start)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeQuota {
		t.Fatalf("err = %v, want ErrCodeQuota", err)
	}
	if aiErr.Retryable {
		t.Error("a quota-exhausted 429 must not be retryable")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (must not retry quota exhaustion)", attempts)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want fast (no Retry-After wait for a non-retryable error)", elapsed)
	}
}

func TestStream_NoRetryAfterWaitOnLastAttempt(t *testing.T) {
	// With the default MaxAttempts=3, WaitOnRetryAfter is eligible to fire
	// after attempt 1 and after attempt 2 (each "not last"), but MUST NOT
	// fire after attempt 3 (the last one) -- there's no attempt 4 whose
	// start we could time it against, so this asserts on TOTAL elapsed
	// instead of a single gap: two honoured waits (~2s each) plus small
	// backoff+jitter lands well under a THIRD 2s wait's worth of headroom.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"try again later","type":"rate_limit_exceeded"}}`))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	start := time.Now()
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (default MaxAttempts)", attempts)
	}
	// Correct: ~2 honoured Retry-After waits (after attempts 1 and 2) plus
	// two small backoff+jitter delays -- comfortably under 5.5s. A bug that
	// also waits after the LAST attempt would add a third 2s wait, pushing
	// this past 6s.
	if elapsed >= 5800*time.Millisecond {
		t.Errorf("elapsed = %v, want well under a 3rd Retry-After wait -- it must not be honoured on the last attempt", elapsed)
	}
	if elapsed < 3500*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~4s (Retry-After honoured on the two non-final attempts)", elapsed)
	}
}

// mustChunk builds one streamed chat-completion chunk carrying a single
// argument-fragment delta.tool_calls entry at index, JSON-marshalled so
// argument text needing escaping (quotes, braces) is never hand-escaped in a
// test fixture.
func mustChunk(t *testing.T, index int, argsFragment string) string {
	t.Helper()
	type fn struct {
		Arguments string `json:"arguments"`
	}
	type tc struct {
		Index    int `json:"index"`
		Function fn  `json:"function"`
	}
	type delta struct {
		ToolCalls []tc `json:"tool_calls"`
	}
	type choice struct {
		Delta delta `json:"delta"`
	}
	type chunk struct {
		Model   string   `json:"model"`
		Choices []choice `json:"choices"`
	}
	b, err := json.Marshal(chunk{Model: "gpt-5", Choices: []choice{{Delta: delta{ToolCalls: []tc{{Index: index, Function: fn{Arguments: argsFragment}}}}}}})
	if err != nil {
		t.Fatalf("mustChunk: %v", err)
	}
	return string(b)
}

func TestStream_ToolCallsAssembledFromMultiChunkParallelDeltas(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Two parallel calls (index 0 and 1); id/name arrive on the first
		// chunk for each index, arguments arrive concatenated across chunks.
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"run_dtql","arguments":""}}]}}]}`)
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"find_bookmarks","arguments":""}}]}}]}`)
		sseWrite(w, mustChunk(t, 0, `{"sql":`))
		sseWrite(w, mustChunk(t, 0, `"select 1"}`))
		sseWrite(w, mustChunk(t, 1, `{"q":"x"}`))
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "gpt-5"})
	req := ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
		Tools: []ai.Tool{
			{Name: "run_dtql", Description: "run a query", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "find_bookmarks", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: ai.ToolChoiceAuto,
	}

	var calls []ai.ToolCall
	var stopReason string
	for ev, err := range p.Stream(context.Background(), req) {
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		switch ev.Type {
		case ai.EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case ai.EventCompleted:
			stopReason = ev.StopReason
		}
	}

	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "call_a" || calls[0].Name != "run_dtql" || string(calls[0].Arguments) != `{"sql":"select 1"}` {
		t.Errorf("call[0] = %+v", calls[0])
	}
	if calls[1].ID != "call_b" || calls[1].Name != "find_bookmarks" || string(calls[1].Arguments) != `{"q":"x"}` {
		t.Errorf("call[1] = %+v", calls[1])
	}
	if stopReason != ai.StopReasonToolCalls {
		t.Errorf("StopReason = %q, want %q", stopReason, ai.StopReasonToolCalls)
	}
	if gotBody.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v, want auto", gotBody.ToolChoice)
	}
	if len(gotBody.Tools) != 2 || gotBody.Tools[0].Function.Name != "run_dtql" {
		t.Errorf("tools = %+v", gotBody.Tools)
	}
}

func TestStream_ToolMessagesRoundTripInRequestBody(t *testing.T) {
	var gotBody chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"gpt-5","choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "gpt-5"})
	req := ai.ChatRequest{
		Reasoning: ai.ReasoningHigh,
		Messages: []ai.Message{
			{Role: ai.RoleUser, Text: "hi"},
			{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "call_a", Name: "run_dtql", Arguments: json.RawMessage(`{"sql":"select 1"}`)}}},
			{Role: ai.RoleTool, ToolResults: []ai.ToolResult{{CallID: "call_a", Content: "1 row"}}},
		},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(gotBody.Messages) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(gotBody.Messages), gotBody.Messages)
	}
	asst := gotBody.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_a" || asst.ToolCalls[0].Function.Name != "run_dtql" {
		t.Errorf("assistant message = %+v", asst)
	}
	toolMsg := gotBody.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_a" || toolMsg.Content != "1 row" {
		t.Errorf("tool message = %+v", toolMsg)
	}
	if gotBody.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want high", gotBody.ReasoningEffort)
	}
}

// TestBuildMessages_DynamicContextStaysOnUserMessageAcrossToolTurns is the
// r1 review's M5 regression test: dynamic context must land on the LAST
// "user" message, never a "tool" role message, across a multi-step
// tool-calling turn.
func TestBuildMessages_DynamicContextStaysOnUserMessageAcrossToolTurns(t *testing.T) {
	req := ai.ChatRequest{
		Context: []ai.ContextBlock{{Kind: ai.ContextDynamic, Text: "it is Tuesday"}},
		Messages: []ai.Message{
			{Role: ai.RoleUser, Text: "how many rows?"},
			{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "call_1", Name: "run_dtql", Arguments: json.RawMessage(`{}`)}}},
			{Role: ai.RoleTool, ToolResults: []ai.ToolResult{{CallID: "call_1", Content: "3 rows"}}},
		},
	}
	msgs := buildMessages(req)

	last := msgs[len(msgs)-1]
	if last.Role != "tool" || strings.Contains(last.Content, "it is Tuesday") {
		t.Fatalf("last message = %+v, want the untouched tool result", last)
	}
	first := msgs[0]
	if first.Role != "user" || !strings.Contains(first.Content, "it is Tuesday") {
		t.Fatalf("first message = %+v, want the dynamic context prefix", first)
	}
}

// TestStream_ReasoningEffortRetriedOnceWithoutItOn400 is the r1 review's M1
// regression test: on a 400 whose error mentions reasoning_effort, the
// Provider retries once WITHOUT it, and remembers not to send it again on
// later Stream calls against the SAME Provider instance.
func TestStream_ReasoningEffortRetriedOnceWithoutItOn400(t *testing.T) {
	var bodies []chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		if body.ReasoningEffort != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'reasoning_effort'","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{Reasoning: ai.ReasoningHigh, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}

	text, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want ok (the retry-without-reasoning_effort request must succeed)", text)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2 (the 400 attempt + one retry)", len(bodies))
	}
	if bodies[0].ReasoningEffort != "high" {
		t.Errorf("first request ReasoningEffort = %q, want high", bodies[0].ReasoningEffort)
	}
	if bodies[1].ReasoningEffort != "" {
		t.Errorf("retry request ReasoningEffort = %q, want empty", bodies[1].ReasoningEffort)
	}

	// A SECOND Stream call on the SAME Provider must not send
	// reasoning_effort at all -- no wasted 400 round trip.
	bodies = nil
	_, _, _, err = ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Collect (2nd call): %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("got %d requests on the 2nd Stream call, want 1 (remembered not to send reasoning_effort)", len(bodies))
	}
	if bodies[0].ReasoningEffort != "" {
		t.Errorf("2nd call ReasoningEffort = %q, want empty (remembered from the 1st call's 400)", bodies[0].ReasoningEffort)
	}
}

// TestStream_UnrelatedUnsupportedParameterDoesNotTriggerReasoningEffortRetry
// is r2's m3 regression test: a 400 naming some OTHER unsupported parameter
// must NOT be treated as a reasoning_effort rejection -- the old, broader
// "unsupported parameter" substring match would have retried (and
// permanently disabled reasoning_effort) for the wrong reason.
func TestStream_UnrelatedUnsupportedParameterDoesNotTriggerReasoningEffortRetry(t *testing.T) {
	var bodies []chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'frequency_penalty'","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{Reasoning: ai.ReasoningHigh, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}

	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err == nil {
		t.Fatal("Collect: want an error (the 400 is unrelated to reasoning_effort and must not be retried into success)")
	}
	if len(bodies) != 1 {
		t.Fatalf("got %d requests, want 1 (no retry for an unrelated unsupported parameter)", len(bodies))
	}
	if bodies[0].ReasoningEffort != "high" {
		t.Errorf("request ReasoningEffort = %q, want high (never sent, so no reason to have dropped it)", bodies[0].ReasoningEffort)
	}
}

// TestStream_ReasoningEffortUnsupportedRememberedPerModel is r2's m3
// regression test: the Provider remembers a reasoning_effort rejection per
// MODEL, not for the whole Provider instance -- a later Stream call naming a
// DIFFERENT model must still try reasoning_effort.
func TestStream_ReasoningEffortUnsupportedRememberedPerModel(t *testing.T) {
	var bodies []chatRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		if body.ReasoningEffort != "" && body.Model == "model-a" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'reasoning_effort'","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	reqA := ai.ChatRequest{Model: "model-a", Reasoning: ai.ReasoningHigh, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}
	reqB := ai.ChatRequest{Model: "model-b", Reasoning: ai.ReasoningHigh, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}

	if _, _, _, err := ai.Collect(p.Stream(context.Background(), reqA)); err != nil {
		t.Fatalf("Collect (model-a): %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests for model-a, want 2 (400 + retry)", len(bodies))
	}

	bodies = nil
	if _, _, _, err := ai.Collect(p.Stream(context.Background(), reqB)); err != nil {
		t.Fatalf("Collect (model-b): %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("got %d requests for model-b, want 1", len(bodies))
	}
	if bodies[0].ReasoningEffort != "high" {
		t.Errorf("model-b ReasoningEffort = %q, want high (model-a's rejection must not carry over to a different model)", bodies[0].ReasoningEffort)
	}
}

func TestStream_ToolCallMissingIDGetsSynthesized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var call *ai.ToolCall
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			call = ev.ToolCall
		}
	}
	if call == nil || call.ID == "" {
		t.Fatalf("call = %+v, want a synthesized non-empty ID", call)
	}
}

// TestStream_ToolCallsWithoutIndexKeyedByIDAssembleAsSeparateCalls covers r1's
// M9: a provider that omits `index` on delta.tool_calls chunks must not have
// two parallel calls collapse into one just because they share the default
// index. A new non-empty id distinct from the call being assembled starts a
// new call; a chunk with no id (or the same id) keeps appending to it.
func TestStream_ToolCallsWithoutIndexKeyedByIDAssembleAsSeparateCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"nyc\"}"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"city\":"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"nyc\"}"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var calls []ai.ToolCall
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want 2 separate calls, not merged into one", calls)
	}
	if calls[0].ID != "call_a" || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"nyc"}` {
		t.Errorf("calls[0] = %+v", calls[0])
	}
	if calls[1].ID != "call_b" || calls[1].Name != "get_time" || string(calls[1].Arguments) != `{"city":"nyc"}` {
		t.Errorf("calls[1] = %+v", calls[1])
	}
}

func TestBuildMessages_EmptyToolCallArgumentsBecomeEmptyObject(t *testing.T) {
	req := ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleUser, Text: "hi"},
			{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "call_1", Name: "noop"}}},
		},
	}
	msgs := buildMessages(req)
	asst := msgs[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("assistant tool call = %+v, want arguments \"{}\"", asst.ToolCalls)
	}
}

func TestBuildMessages_ToolResultErrorPrefixed(t *testing.T) {
	req := ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleTool, ToolResults: []ai.ToolResult{
				{CallID: "call_1", Content: "boom", IsError: true},
				{CallID: "call_2", Content: "Error: already prefixed", IsError: true},
				{CallID: "call_3", Content: "ok", IsError: false},
			}},
		},
	}
	msgs := buildMessages(req)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if msgs[0].Content != "Error: boom" {
		t.Errorf("msgs[0].Content = %q, want an Error: prefix added", msgs[0].Content)
	}
	if msgs[1].Content != "Error: already prefixed" {
		t.Errorf("msgs[1].Content = %q, want unchanged (already prefixed)", msgs[1].Content)
	}
	if msgs[2].Content != "ok" {
		t.Errorf("msgs[2].Content = %q, want unchanged (not an error)", msgs[2].Content)
	}
}

func TestRenderDynamic_NamedBlock(t *testing.T) {
	got := renderDynamic([]ai.ContextBlock{
		{Kind: ai.ContextDynamic, Name: "today", Text: "it is Tuesday"},
	})
	if !strings.Contains(got, "# today\n") {
		t.Errorf("renderDynamic = %q, want a heading for the named block", got)
	}
}

func TestRenderDynamic_MultipleBlocksSeparated(t *testing.T) {
	// A second dynamic block must be joined with a blank-line separator
	// (dyn.Len() > 0 on the second iteration).
	got := renderDynamic([]ai.ContextBlock{
		{Kind: ai.ContextDynamic, Text: "first"},
		{Kind: ai.ContextDynamic, Text: "second"},
	})
	if !strings.Contains(got, "first\n\nsecond") {
		t.Errorf("renderDynamic = %q, want first and second blocks separated by a blank line", got)
	}
}

func TestRenderDynamic_Empty(t *testing.T) {
	if got := renderDynamic(nil); got != "" {
		t.Errorf("renderDynamic(nil) = %q, want empty", got)
	}
	if got := renderDynamic([]ai.ContextBlock{{Kind: ai.ContextStatic, Text: "static only"}}); got != "" {
		t.Errorf("renderDynamic(static-only) = %q, want empty", got)
	}
}

func TestExtractJSON_FencedAndPlain(t *testing.T) {
	if got := extractJSON("```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("extractJSON(fenced json) = %q", got)
	}
	if got := extractJSON("```\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("extractJSON(fenced) = %q", got)
	}
	if got := extractJSON(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("extractJSON(plain) = %q", got)
	}
}

func TestToolChoiceWire_AllCases(t *testing.T) {
	if got := toolChoiceWire(""); got != "auto" {
		t.Errorf("toolChoiceWire(\"\") = %v", got)
	}
	if got := toolChoiceWire(ai.ToolChoiceAuto); got != "auto" {
		t.Errorf("toolChoiceWire(auto) = %v", got)
	}
	if got := toolChoiceWire(ai.ToolChoiceNone); got != "none" {
		t.Errorf("toolChoiceWire(none) = %v", got)
	}
	if got := toolChoiceWire(ai.ToolChoiceRequired); got != "required" {
		t.Errorf("toolChoiceWire(required) = %v", got)
	}
	got := toolChoiceWire("my_tool")
	nt, ok := got.(namedToolChoice)
	if !ok || nt.Type != "function" || nt.Function.Name != "my_tool" {
		t.Errorf("toolChoiceWire(named) = %+v", got)
	}
}

func TestToAIError_PassesThroughAIErrorWhenNotCanceled(t *testing.T) {
	src := &ai.Error{Code: ai.ErrCodeUpstream, Message: "boom"}
	got := toAIError(context.Background(), src)
	if got != src {
		t.Errorf("toAIError = %+v, want the original *ai.Error unchanged", got)
	}
}

func TestToAIError_AIErrorWithCanceledCtxBecomesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := toAIError(ctx, &ai.Error{Code: ai.ErrCodeUpstream, Message: "boom"})
	if got.Code != ai.ErrCodeCanceled {
		t.Errorf("Code = %q, want canceled", got.Code)
	}
}

func TestToAIError_PlainErrorBecomesRetryableUpstream(t *testing.T) {
	got := toAIError(context.Background(), errors.New("dial tcp: connection refused"))
	if got.Code != ai.ErrCodeUpstream || !got.Retryable {
		t.Errorf("got = %+v, want retryable upstream", got)
	}
}

func TestToAIError_ContextErrorsBecomeCanceled(t *testing.T) {
	if got := toAIError(context.Background(), context.Canceled); got.Code != ai.ErrCodeCanceled {
		t.Errorf("got = %+v", got)
	}
	if got := toAIError(context.Background(), context.DeadlineExceeded); got.Code != ai.ErrCodeCanceled {
		t.Errorf("got = %+v", got)
	}
}

func TestIsUnsupportedReasoningEffortError_FalseCases(t *testing.T) {
	if isUnsupportedReasoningEffortError(errors.New("plain error")) {
		t.Error("a plain (non-*ai.Error) error must never match")
	}
	if isUnsupportedReasoningEffortError(&ai.Error{Code: ai.ErrCodeUpstream, Message: "reasoning_effort is bad"}) {
		t.Error("a non-Invalid code must never match, even if the message mentions reasoning_effort")
	}
	if isUnsupportedReasoningEffortError(&ai.Error{Code: ai.ErrCodeInvalid, Message: "unrelated field bad"}) {
		t.Error("an Invalid error that doesn't name reasoning_effort must not match")
	}
}

func TestHTTPStatusError_FallbackMessages(t *testing.T) {
	// Non-JSON body: apiErr.Error.Message stays empty, so msg falls back to
	// the trimmed raw body.
	got := httpStatusError(http.StatusInternalServerError, []byte("  plain text error  "))
	if got.Message != "plain text error" {
		t.Errorf("Message = %q, want the trimmed raw body", got.Message)
	}
	// Empty body and unparseable/empty JSON: falls back to "HTTP <status>".
	got2 := httpStatusError(http.StatusBadGateway, nil)
	if got2.Message != "HTTP 502" {
		t.Errorf("Message = %q, want \"HTTP 502\"", got2.Message)
	}
}

func TestDoRequest_InvalidURLFails(t *testing.T) {
	// A BaseURL containing a raw control character makes
	// http.NewRequestWithContext's underlying url.Parse fail before any
	// network I/O happens.
	p := &Provider{cfg: Config{BaseURL: "http://example.com/\x7f", HTTPClient: http.DefaultClient}}
	_, err := p.doRequest(context.Background(), []byte(`{}`), true)
	if err == nil {
		t.Fatal("expected an error building the request")
	}
}

func TestDoRequest_ContextCanceledBeforeResponse(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond until the test is done
	}))
	defer func() { close(block); srv.Close() }()
	p := &Provider{cfg: Config{BaseURL: srv.URL, HTTPClient: srv.Client()}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.doRequest(ctx, []byte(`{}`), true)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ErrCodeCanceled", err)
	}
}

func TestDoRequest_ConnectionErrorIsRetryableUpstream(t *testing.T) {
	// Point at a closed local listener so the HTTP client's Do() fails with
	// a plain connection error, not context cancellation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening
	p := &Provider{cfg: Config{BaseURL: url, HTTPClient: http.DefaultClient}}
	_, err := p.doRequest(context.Background(), []byte(`{}`), true)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !aiErr.Retryable {
		t.Fatalf("err = %v, want a retryable upstream error", err)
	}
}

func TestStream_PayloadMarshalErrorIsFatal(t *testing.T) {
	// json.Marshal validates a json.RawMessage field's bytes; an invalid
	// ResponseSchema makes marshalling the request body itself fail before
	// any HTTP call is attempted.
	p := New(Config{BaseURL: "http://unused.example", Model: "m"})
	req := ai.ChatRequest{ResponseSchema: json.RawMessage("not json")}
	var lastEvent ai.Event
	var lastErr error
	n := 0
	for ev, err := range p.Stream(context.Background(), req) {
		n++
		lastEvent, lastErr = ev, err
	}
	if lastErr == nil {
		t.Fatal("expected a fatal marshal error")
	}
	if n != 1 || lastEvent.Type != ai.EventError {
		t.Fatalf("n=%d lastEvent=%+v, want exactly one fatal EventError yield", n, lastEvent)
	}
	var aiErr *ai.Error
	if !errors.As(lastErr, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		t.Fatalf("lastErr = %v, want ErrCodeInvalid", lastErr)
	}
}

func TestStream_EarlyBreakAfterStartedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"hi"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	n := 0
	for range p.Stream(context.Background(), ai.ChatRequest{}) {
		n++
		if n == 1 {
			break // stop right after EventStarted
		}
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1 (consumer break must stop iteration promptly)", n)
	}
}

func TestStream_EarlyBreakAfterUsageEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		sseWrite(w, `{"choices":[{"delta":{"content":"should not appear"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	n := 0
	var types []ai.EventType
	for ev, _ := range p.Stream(context.Background(), ai.ChatRequest{}) {
		n++
		types = append(types, ev.Type)
		if ev.Type == ai.EventUsage {
			break
		}
	}
	if len(types) != 2 || types[1] != ai.EventUsage {
		t.Fatalf("types = %v, want [started, usage]", types)
	}
}

func TestStream_EarlyBreakAfterStructuredEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"{\"a\":1}"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{ResponseSchema: json.RawMessage(`{"type":"object"}`)}
	var types []ai.EventType
	for ev, _ := range p.Stream(context.Background(), req) {
		types = append(types, ev.Type)
		if ev.Type == ai.EventStructured {
			break
		}
	}
	if len(types) == 0 || types[len(types)-1] != ai.EventStructured {
		t.Fatalf("types = %v, want to end at EventStructured", types)
	}
}

func TestStream_EarlyBreakAfterToolCallEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"noop2","arguments":"{}"}}]}}]}`)
		sseWrite(w, `{"model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var calls []ai.ToolCall
	for ev, _ := range p.Stream(context.Background(), ai.ChatRequest{}) {
		if ev.Type == ai.EventToolCall {
			calls = append(calls, *ev.ToolCall)
			break // stop after the FIRST tool call, before the second is yielded
		}
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want exactly 1 (consumer broke after the first)", calls)
	}
}

func TestStream_EarlyBreakAfterTextDeltaEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"first"}}]}`)
		sseWrite(w, `{"choices":[{"delta":{"content":"should not appear"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var texts []string
	for ev, _ := range p.Stream(context.Background(), ai.ChatRequest{}) {
		if ev.Type == ai.EventTextDelta {
			texts = append(texts, ev.Text)
			break // stop right after the FIRST text.delta
		}
	}
	if len(texts) != 1 || texts[0] != "first" {
		t.Fatalf("texts = %v, want exactly [\"first\"]", texts)
	}
}

// hijackAbortWriter is an http.Handler helper: it writes a partial SSE
// response, then hijacks and abruptly closes the raw connection (no proper
// TLS/HTTP close), so the client's body Read returns a transport error
// (not a clean EOF) -- the only realistic way to drive bufio.Scanner's
// sc.Err() path (as opposed to a well-formed truncation, which is the
// "no [DONE] marker" case already covered elsewhere).
func TestStream_ScannerTransportErrorIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		_ = conn.Close() // abrupt close: the client sees a transport error, not EOF
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err == nil {
		t.Fatal("expected a fatal transport error")
	}
}

func TestStream_EmptyDataLineSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: \n\n") // an empty data: line must be skipped, not error
		w.(http.Flusher).Flush()
		sseWrite(w, `{"choices":[{"delta":{"content":"hi"}}]}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_ReasoningTokensFromUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":7}}}`)
		sseWrite(w, "[DONE]")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, usage, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.ReasoningTokens != 7 {
		t.Fatalf("usage = %+v, want ReasoningTokens=7", usage)
	}
}
