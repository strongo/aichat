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
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	// blocks: [system, staticA, staticB(cache), dynamic]
	if len(gotBody.System) != 4 {
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
	if !strings.Contains(gotBody.System[3].Text, "dyn now") || gotBody.System[3].CacheControl != nil {
		t.Errorf("System[3] (dynamic, must follow static, uncached) = %+v", gotBody.System[3])
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

func TestNew_PanicsWithoutBaseURL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for missing BaseURL")
		}
	}()
	New(Config{})
}
