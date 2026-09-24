package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
)

func sseWrite(w http.ResponseWriter, data string) {
	_, _ = io.WriteString(w, "event: msg\ndata: "+data+"\n\n")
	w.(http.Flusher).Flush()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNew_PanicsWithoutBaseURL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	New(Config{})
}

func TestNew_DefaultsHTTPClient(t *testing.T) {
	p := New(Config{BaseURL: "http://example.invalid"})
	if p.cfg.HTTPClient != http.DefaultClient {
		t.Fatalf("HTTPClient = %v, want http.DefaultClient", p.cfg.HTTPClient)
	}
}

func TestName(t *testing.T) {
	p := New(Config{BaseURL: "http://x"})
	if p.Name() != "openai-responses" {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestStream_BasicTextToolCallAndUsage(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Extra") != "1" {
			t.Errorf("X-Extra header not set")
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.created","response":{"id":"r1","status":"in_progress"}}`)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"Hello"}`)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":", world"}`)
		sseWrite(w, `{"type":"response.output_item.added","output_index":1,"item":{"id":"fc1","type":"function_call","call_id":"call_1","name":"get_weather"}}`)
		sseWrite(w, `{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{\"city\":"}`)
		sseWrite(w, `{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"\"Rome\"}"}`)
		sseWrite(w, `{"type":"response.function_call_arguments.done","item_id":"fc1","arguments":"{\"city\":\"Rome\"}"}`)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}`)
	}))
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Model: "gpt-5", Headers: map[string]string{"X-Extra": "1"}})

	req := ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}
	var events []ai.Event
	var finalErr error
	for ev, err := range p.Stream(context.Background(), req) {
		if err != nil {
			finalErr = err
			break
		}
		events = append(events, ev)
	}
	if finalErr != nil {
		t.Fatalf("Stream: %v", finalErr)
	}
	if gotBody.Model != "gpt-5" || !gotBody.Stream {
		t.Errorf("gotBody = %+v", gotBody)
	}
	if len(gotBody.Input) != 1 || gotBody.Input[0].Role != "user" || gotBody.Input[0].Content != "hi" {
		t.Errorf("gotBody.Input = %+v", gotBody.Input)
	}

	var text string
	var gotToolCall *ai.ToolCall
	var gotUsage *ai.Usage
	var completed *ai.Event
	var startedSeen bool
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case ai.EventStarted:
			startedSeen = true
			if ev.Provider != "openai-responses" || ev.Model != "gpt-5" {
				t.Errorf("Started event = %+v", ev)
			}
		case ai.EventTextDelta:
			text += ev.Text
		case ai.EventToolCall:
			gotToolCall = ev.ToolCall
		case ai.EventUsage:
			gotUsage = ev.Usage
		case ai.EventCompleted:
			e := ev
			completed = &e
		}
	}
	if !startedSeen {
		t.Fatal("EventStarted not seen")
	}
	if text != "Hello, world" {
		t.Errorf("text = %q", text)
	}
	if gotToolCall == nil || gotToolCall.ID != "call_1" || gotToolCall.Name != "get_weather" || string(gotToolCall.Arguments) != `{"city":"Rome"}` {
		t.Errorf("gotToolCall = %+v", gotToolCall)
	}
	if gotUsage == nil || gotUsage.InputTokens != 10 || gotUsage.OutputTokens != 5 || gotUsage.CacheReadTokens != 3 || gotUsage.ReasoningTokens != 2 {
		t.Errorf("gotUsage = %+v", gotUsage)
	}
	if completed == nil || completed.StopReason != ai.StopReasonToolCalls {
		t.Errorf("completed = %+v", completed)
	}
	if completed.Usage == nil || completed.Usage.InputTokens != 10 {
		t.Errorf("completed.Usage = %+v", completed.Usage)
	}
}

func TestStream_ModelAutoUsesConfigDefault(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body responseRequestBody
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
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

func TestStream_InstructionsHoldOnlyStaticContext(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
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
	if !strings.Contains(gotBody.Instructions, "You are helpful.") || !strings.Contains(gotBody.Instructions, "static instructions") {
		t.Errorf("Instructions = %q", gotBody.Instructions)
	}
	if strings.Contains(gotBody.Instructions, "now=2026") {
		t.Errorf("Instructions leaked dynamic context: %q", gotBody.Instructions)
	}
	if len(gotBody.Input) != 1 || !strings.Contains(gotBody.Input[0].Content, "now=2026") || !strings.HasSuffix(gotBody.Input[0].Content, "hi") {
		t.Errorf("Input = %+v", gotBody.Input)
	}
}

func TestStream_DynamicContextDroppedWithoutUserMessage(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Context: []ai.ContextBlock{{Scope: "s", Kind: ai.ContextDynamic, Name: "now", Text: "now=2026"}},
		Messages: []ai.Message{
			{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}}},
			{Role: ai.RoleTool, ToolResults: []ai.ToolResult{{CallID: "c1", Content: "ok"}}},
		},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range gotBody.Input {
		if strings.Contains(it.Content, "now=2026") || strings.Contains(it.Output, "now=2026") {
			t.Errorf("dynamic context leaked into %+v", it)
		}
	}
}

func TestStream_ToolCallsAndResultsRoundtrip(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Messages: []ai.Message{
			{Role: ai.RoleUser, Text: "weather?"},
			{Role: ai.RoleAssistant, Text: "checking", ToolCalls: []ai.ToolCall{
				{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Rome"}`)},
				{ID: "call_2", Name: "noop"},
			}},
			{Role: ai.RoleTool, ToolResults: []ai.ToolResult{
				{CallID: "call_1", Content: "sunny"},
				{CallID: "call_2", Content: "boom", IsError: true},
			}},
		},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody.Input) != 6 {
		t.Fatalf("Input len = %d, want 6: %+v", len(gotBody.Input), gotBody.Input)
	}
	if gotBody.Input[0].Role != "user" || gotBody.Input[0].Content != "weather?" {
		t.Errorf("Input[0] = %+v", gotBody.Input[0])
	}
	if gotBody.Input[1].Role != "assistant" || gotBody.Input[1].Content != "checking" {
		t.Errorf("Input[1] = %+v", gotBody.Input[1])
	}
	if gotBody.Input[2].Type != "function_call" || gotBody.Input[2].CallID != "call_1" || gotBody.Input[2].Arguments != `{"city":"Rome"}` {
		t.Errorf("Input[2] = %+v", gotBody.Input[2])
	}
	if gotBody.Input[3].Type != "function_call" || gotBody.Input[3].CallID != "call_2" || gotBody.Input[3].Arguments != `{}` {
		t.Errorf("Input[3] = %+v", gotBody.Input[3])
	}
	if gotBody.Input[4].Type != "function_call_output" || gotBody.Input[4].CallID != "call_1" || gotBody.Input[4].Output != "sunny" {
		t.Errorf("Input[4] = %+v", gotBody.Input[4])
	}
	if gotBody.Input[5].Type != "function_call_output" || gotBody.Input[5].CallID != "call_2" || gotBody.Input[5].Output != "Error: boom" {
		t.Errorf("Input[5] = %+v", gotBody.Input[5])
	}
}

func TestStream_ToolsAndToolChoice(t *testing.T) {
	cases := []struct {
		name    string
		choice  string
		wantAny any
	}{
		{"empty", "", "auto"},
		{"auto", ai.ToolChoiceAuto, "auto"},
		{"none", ai.ToolChoiceNone, "none"},
		{"required", ai.ToolChoiceRequired, "required"},
		{"named", "get_weather", map[string]any{"type": "function", "name": "get_weather"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody responseRequestBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &gotBody)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
			}))
			defer srv.Close()
			p := New(Config{BaseURL: srv.URL, Model: "m"})
			req := ai.ChatRequest{
				Messages:   []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
				Tools:      []ai.Tool{{Name: "get_weather", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
				ToolChoice: tc.choice,
			}
			_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
			if err != nil {
				t.Fatal(err)
			}
			if len(gotBody.Tools) != 1 || gotBody.Tools[0].Type != "function" || gotBody.Tools[0].Name != "get_weather" || gotBody.Tools[0].Strict {
				t.Errorf("Tools = %+v", gotBody.Tools)
			}
			gotJSON, _ := json.Marshal(gotBody.ToolChoice)
			wantJSON, _ := json.Marshal(tc.wantAny)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("ToolChoice = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestStream_ReasoningAndMaxTokens(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Messages:  []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
		Reasoning: ai.ReasoningHigh,
		MaxTokens: 512,
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Reasoning == nil || gotBody.Reasoning.Effort != "high" {
		t.Errorf("Reasoning = %+v", gotBody.Reasoning)
	}
	if gotBody.MaxOutputTokens != 512 {
		t.Errorf("MaxOutputTokens = %d", gotBody.MaxOutputTokens)
	}
}

func TestStream_NoReasoningWhenEmpty(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Reasoning != nil {
		t.Errorf("Reasoning = %+v, want nil", gotBody.Reasoning)
	}
}

func TestStream_ResponseSchemaStrictDefaultAndFencedJSON(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fence := "```json\n"
		sseWrite(w, mustJSON(t, map[string]string{"type": "response.output_text.delta", "item_id": "it1", "delta": fence}))
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"{\"ok\":true}\n"}`)
		closeFence := "```"
		sseWrite(w, mustJSON(t, map[string]string{"type": "response.output_text.delta", "item_id": "it1", "delta": closeFence}))
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	_, structured, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Text == nil || gotBody.Text.Format.Type != "json_schema" || !gotBody.Text.Format.Strict {
		t.Errorf("Text = %+v", gotBody.Text)
	}
	if string(structured) != `{"ok":true}` {
		t.Errorf("structured = %s", structured)
	}
}

func TestStream_StrictSchemaOptOut(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	no := false
	req := ai.ChatRequest{
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		StrictSchema:   &no,
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Text == nil || gotBody.Text.Format.Strict {
		t.Errorf("Text = %+v, want Strict=false", gotBody.Text)
	}
}

func TestStream_TruncatedStructuredResponseFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"{\"partial"}`)
		sseWrite(w, `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err == nil {
		t.Fatal("expected error")
	}
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Errorf("err = %v", err)
	}
}

func TestStream_IncompleteMaxOutputTokensStopReasonLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"partial"}`)
		sseWrite(w, `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","usage":{"input_tokens":1,"output_tokens":2},"incomplete_details":{"reason":"max_output_tokens"}}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var completed *ai.Event
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventCompleted {
			e := ev
			completed = &e
		}
	}
	if completed == nil || completed.StopReason != ai.StopReasonLength {
		t.Errorf("completed = %+v", completed)
	}
}

func TestStream_IncompleteOtherReasonStopReasonEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var completed *ai.Event
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventCompleted {
			e := ev
			completed = &e
		}
	}
	if completed == nil || completed.StopReason != ai.StopReasonEnd {
		t.Errorf("completed = %+v", completed)
	}
}

func TestStream_ResponseFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || aiErr.Message != "boom" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_ResponseFailedNoErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.failed","response":{"id":"r1","status":"failed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || aiErr.Message != "openairesponses: response failed" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_TopLevelErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"error","code":"bad","message":"oops"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || aiErr.Message != "oops" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_MalformedChunkFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{not json`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Errorf("err = %v", err)
	}
}

func TestStream_NoTerminalEventFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"hi"}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !strings.Contains(aiErr.Message, "stream truncated") {
		t.Errorf("err = %v", err)
	}
}

func TestStream_UnknownEventIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.in_progress","response":{"id":"r1","status":"in_progress"}}`)
		sseWrite(w, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`)
		sseWrite(w, "")
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_ArgsDeltaBeforeAddedTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"{}"}`)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","call_id":"","name":""}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var toolCall *ai.ToolCall
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			toolCall = ev.ToolCall
		}
	}
	if toolCall == nil || toolCall.ID != "call_0" || string(toolCall.Arguments) != "{}" {
		t.Errorf("toolCall = %+v", toolCall)
	}
}

func TestStream_MessageOutputItemDoneIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_item.added","item":{"id":"m1","type":"message"}}`)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"m1","type":"message"}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var sawToolCall bool
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			sawToolCall = true
		}
	}
	if sawToolCall {
		t.Error("unexpected tool call for a message output item")
	}
}

func TestStream_EmptyDeltaIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":""}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	text, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Errorf("text = %q", text)
	}
}

func TestStream_NoUsageOnCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, usage, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
}

// --- HTTP-level error handling ---------------------------------------------

func TestStream_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeAuth || aiErr.Message != "bad key" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeAuth || aiErr.Message != "HTTP 403" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_QuotaExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"no credit","type":"insufficient_quota"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeQuota {
		t.Errorf("err = %v", err)
	}
}

func TestStream_RateLimitedThenSuccess(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if atomic.LoadInt32(&attempts) < 2 {
		t.Fatalf("attempts = %d, want >= 2", attempts)
	}
}

func TestStream_ServerErrorRetryExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Errorf("err = %v", err)
	}
}

func TestStream_BadRequestInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		t.Errorf("err = %v", err)
	}
}

func TestStream_ContextCanceledBeforeFirstByte(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		srv.Close()
	}()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := ai.Collect(p.Stream(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled || aiErr.Retryable {
		t.Errorf("err = %v", err)
	}
}

func TestStream_ContextCanceledMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"hi"}`)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	for ev, e := range p.Stream(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if ev.Type == ai.EventTextDelta {
			cancel()
		}
		if e != nil {
			err = e
			break
		}
	}
	close(release)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		// A mid-stream cancellation surfaces as a scanner read error, which
		// toAIError classifies as ErrCodeUpstream unless ctx.Err() already
		// fired -- either is acceptable here; assert only that it is fatal.
		if !errors.As(err, &aiErr) {
			t.Fatalf("err = %v, want *ai.Error", err)
		}
	}
}

// --- consumer-stops-iterating early-return branches ------------------------

func TestStream_ConsumerStopsAtStarted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	n := 0
	for range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		n++
		break
	}
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
}

func TestStream_ConsumerStopsAtTextDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"hi"}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	n := 0
	for ev := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		n++
		if ev.Type == ai.EventTextDelta {
			break
		}
	}
	if n != 2 {
		t.Fatalf("n = %d", n)
	}
}

func TestStream_ConsumerStopsAtUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	for ev := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if ev.Type == ai.EventUsage {
			return
		}
	}
}

func TestStream_ConsumerStopsAtStructured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_text.delta","item_id":"it1","delta":"{}"}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}, ResponseSchema: json.RawMessage(`{}`)}
	for ev := range p.Stream(context.Background(), req) {
		if ev.Type == ai.EventStructured {
			return
		}
	}
}

func TestStream_ConsumerStopsAtToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_item.added","item":{"id":"fc1","type":"function_call","call_id":"c1","name":"f"}}`)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","call_id":"c1","name":"f","arguments":"{}"}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	for ev := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if ev.Type == ai.EventToolCall {
			return
		}
	}
}

func TestStream_MarshalErrorFatal(t *testing.T) {
	orig := marshalJSON
	marshalJSON = func(v any) ([]byte, error) { return nil, errors.New("boom") }
	defer func() { marshalJSON = orig }()

	p := New(Config{BaseURL: "http://example.invalid", Model: "m"})
	req := ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid || aiErr.Message != "boom" {
		t.Errorf("err = %v", err)
	}
}

func TestStream_FinalizeWithoutPriorAdd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}"}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var toolCall *ai.ToolCall
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			toolCall = ev.ToolCall
		}
	}
	if toolCall == nil || toolCall.ID != "c1" || toolCall.Name != "f" || string(toolCall.Arguments) != `{"x":1}` {
		t.Errorf("toolCall = %+v", toolCall)
	}
}

func TestStream_ToolCallWithNoArgumentsDefaultsEmptyObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.output_item.added","item":{"id":"fc1","type":"function_call","call_id":"c1","name":"f"}}`)
		sseWrite(w, `{"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","call_id":"c1","name":"f"}}`)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	var toolCall *ai.ToolCall
	for ev, err := range p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == ai.EventToolCall {
			toolCall = ev.ToolCall
		}
	}
	if toolCall == nil || string(toolCall.Arguments) != "{}" {
		t.Errorf("toolCall = %+v", toolCall)
	}
}

func TestStream_MultipleDynamicContextBlocks(t *testing.T) {
	var gotBody responseRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, Model: "m"})
	req := ai.ChatRequest{
		Context: []ai.ContextBlock{
			{Scope: "s", Kind: ai.ContextDynamic, Name: "", Text: "first"},
			{Scope: "s", Kind: ai.ContextDynamic, Name: "second", Text: "second-text"},
		},
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
	}
	_, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBody.Input) != 1 || !strings.Contains(gotBody.Input[0].Content, "first") || !strings.Contains(gotBody.Input[0].Content, "# second\nsecond-text") {
		t.Errorf("Input = %+v", gotBody.Input)
	}
}

func TestCallAssembler_SetArgsOnUnknownItem(t *testing.T) {
	a := newCallAssembler()
	a.setArgs("x", `{"a":1}`)
	calls := a.calls()
	if len(calls) != 1 || string(calls[0].Arguments) != `{"a":1}` {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestCallAssembler_AddIdempotent(t *testing.T) {
	a := newCallAssembler()
	a.add("x", "c1", "f1")
	a.add("x", "c2", "f2")
	if len(a.order) != 1 || a.byID["x"].ID != "c1" {
		t.Fatalf("byID = %+v", a.byID)
	}
}

func TestUsageFromWire_Nil(t *testing.T) {
	if usageFromWire(nil) != nil {
		t.Fatal("usageFromWire(nil) != nil")
	}
}

func TestExtractJSON_NoFence(t *testing.T) {
	if got := extractJSON(`{"a":1}`); got != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestHTTPStatusError_BodyFallback(t *testing.T) {
	err := httpStatusError(400, []byte("plain text body"))
	if err.Message != "plain text body" {
		t.Fatalf("Message = %q", err.Message)
	}
	err2 := httpStatusError(400, nil)
	if err2.Message != "HTTP 400" {
		t.Fatalf("Message = %q", err2.Message)
	}
}

func TestToAIError_WrapsContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	e := toAIError(ctx, context.DeadlineExceeded)
	if e.Code != ai.ErrCodeCanceled {
		t.Fatalf("Code = %q", e.Code)
	}
}

func TestToAIError_PlainError(t *testing.T) {
	e := toAIError(context.Background(), errors.New("boom"))
	if e.Code != ai.ErrCodeUpstream || !e.Retryable {
		t.Fatalf("e = %+v", e)
	}
}

func TestStream_ConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens at url anymore -> connection refused, no ctx cancellation
	p := New(Config{BaseURL: url, Model: "m"})
	_, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !aiErr.Retryable {
		t.Errorf("err = %v", err)
	}
}

func TestDoRequest_NewRequestError(t *testing.T) {
	p := New(Config{BaseURL: "http://[::1]:namedport", Model: "m"})
	_, err := p.doRequest(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error")
	}
}
