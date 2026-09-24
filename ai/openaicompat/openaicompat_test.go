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

func TestStream_SystemAndContextOrdering(t *testing.T) {
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
	iStatic := strings.Index(sysMsg.Content, "static instructions")
	iDynamic := strings.Index(sysMsg.Content, "now=2026")
	if iStatic < 0 || iDynamic < 0 || iStatic > iDynamic {
		t.Errorf("static block must precede dynamic block in system message: %q", sysMsg.Content)
	}
	if !strings.Contains(sysMsg.Content, "You are helpful.") {
		t.Errorf("system prompt missing: %q", sysMsg.Content)
	}
	if gotMessages[1].Role != "user" || gotMessages[1].Content != "hi" {
		t.Errorf("gotMessages[1] = %+v", gotMessages[1])
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
