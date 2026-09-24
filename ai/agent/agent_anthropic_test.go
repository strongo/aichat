package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/anthropic"
)

func sseWrite(w http.ResponseWriter, event, data string) {
	_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
	w.(http.Flusher).Flush()
}

// TestLoop_AnthropicThinkingBlockReplayedBeforeToolUse is the pinned r0
// regression test: when extended thinking is enabled, Anthropic requires
// the thinking (and redacted_thinking) blocks of the assistant turn that
// requested a tool call to be replayed UNMODIFIED (with their signature) in
// the next request, preceding the tool_use block — dropping them 400s the
// second step. It drives ai/agent.Loop over the REAL ai/anthropic.Provider
// against an httptest server that plays a 2-step tool-use script with
// thinking enabled, and inspects the second request's wire body.
func TestLoop_AnthropicThinkingBlockReplayedBeforeToolUse(t *testing.T) {
	var requestBodies []map[string]any
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		requestBodies = append(requestBodies, body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if requestCount == 1 {
			// Step 1: thinking, then a tool_use call.
			sseWrite(w, "message_start", `{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":10}}}`)
			sseWrite(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)
			sseWrite(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me check the "}}`)
			sseWrite(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"data first."}}`)
			sseWrite(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc123"}}`)
			sseWrite(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			sseWrite(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"run_dtql"}}`)
			sseWrite(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"sql\":\"select 1\"}"}}`)
			sseWrite(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
			sseWrite(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`)
			sseWrite(w, "message_stop", `{"type":"message_stop"}`)
			return
		}

		// Step 2: plain text completion.
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"the answer is 1"}}`)
		sseWrite(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	provider := anthropic.New(anthropic.Config{BaseURL: srv.URL, APIKey: "sk-ant", Model: "claude-haiku-4-5-20251001"})
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"run_dtql": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID, Content: "1 row"}, nil
			},
		},
	}

	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{
		Reasoning: ai.ReasoningMedium,
		Messages:  []ai.Message{{Role: ai.RoleUser, Text: "how many?"}},
		Tools:     []ai.Tool{{Name: "run_dtql", Schema: json.RawMessage(`{"type":"object"}`)}},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want 2", requestCount)
	}

	// The second request's assistant message (the one carrying the
	// tool_use call from step 1) must lead with the thinking block,
	// signature intact, BEFORE the tool_use block.
	messages, _ := requestBodies[1]["messages"].([]any)
	var assistantMsg map[string]any
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistantMsg = mm
			break
		}
	}
	if assistantMsg == nil {
		t.Fatalf("no assistant message in second request body: %+v", requestBodies[1])
	}
	content, _ := assistantMsg["content"].([]any)
	if len(content) < 2 {
		t.Fatalf("assistant content = %+v, want at least [thinking, tool_use]", content)
	}
	thinkingBlock := content[0].(map[string]any)
	if thinkingBlock["type"] != "thinking" {
		t.Fatalf("content[0].type = %v, want thinking (must precede tool_use)", thinkingBlock["type"])
	}
	if thinkingBlock["signature"] != "sig-abc123" {
		t.Errorf("thinking signature = %v, want unmodified sig-abc123", thinkingBlock["signature"])
	}
	if thinkingBlock["thinking"] != "Let me check the data first." {
		t.Errorf("thinking text = %v, want the concatenated deltas unmodified", thinkingBlock["thinking"])
	}
	var sawToolUseAfterThinking bool
	for _, b := range content[1:] {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			sawToolUseAfterThinking = true
			if bm["id"] != "call_1" {
				t.Errorf("tool_use id = %v, want call_1", bm["id"])
			}
		}
	}
	if !sawToolUseAfterThinking {
		t.Error("expected a tool_use block after the thinking block")
	}

	// The request must also have thinking enabled (medium budget).
	thinking, _ := requestBodies[1]["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(4096) {
		t.Errorf("thinking config on 2nd request = %+v, want enabled/4096", thinking)
	}
}

// TestLoop_AnthropicNoArgToolCallReplaysNonEmptyInput is the pinned r2
// regression test for X1 (blocker): a no-argument tool call streams zero
// input_json_delta chunks, so its accumulated arguments are empty. Captured
// into ProviderState and replayed verbatim on the next request, an empty
// "input" would be omitted from the wire entirely (providerStateBlock's
// omitempty) — Anthropic requires a tool_use block to always carry "input",
// and a missing key 400s the request. Drives ai/agent.Loop over the REAL
// ai/anthropic.Provider through a 2-step no-arg tool-use script and asserts
// the second request's replayed tool_use block carries a non-empty object,
// not a missing/empty "input".
func TestLoop_AnthropicNoArgToolCallReplaysNonEmptyInput(t *testing.T) {
	var requestBodies []map[string]any
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		requestBodies = append(requestBodies, body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if requestCount == 1 {
			// Step 1: a tool_use call with NO input_json_delta chunks at
			// all — the no-argument case.
			sseWrite(w, "message_start", `{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","usage":{"input_tokens":10}}}`)
			sseWrite(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"list_tables"}}`)
			sseWrite(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			sseWrite(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`)
			sseWrite(w, "message_stop", `{"type":"message_stop"}`)
			return
		}

		// Step 2: plain text completion.
		sseWrite(w, "content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"3 tables"}}`)
		sseWrite(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
		sseWrite(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()

	provider := anthropic.New(anthropic.Config{BaseURL: srv.URL, APIKey: "sk-ant", Model: "claude-haiku-4-5-20251001"})
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"list_tables": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID, Content: "3 tables"}, nil
			},
		},
	}

	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "how many tables?"}},
		Tools:    []ai.Tool{{Name: "list_tables", Schema: json.RawMessage(`{"type":"object"}`)}},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want 2", requestCount)
	}

	messages, _ := requestBodies[1]["messages"].([]any)
	var assistantMsg map[string]any
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" {
			assistantMsg = mm
			break
		}
	}
	if assistantMsg == nil {
		t.Fatalf("no assistant message in second request body: %+v", requestBodies[1])
	}
	content, _ := assistantMsg["content"].([]any)
	var toolUse map[string]any
	for _, b := range content {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			toolUse = bm
		}
	}
	if toolUse == nil {
		t.Fatalf("assistant content = %+v, want a replayed tool_use block", content)
	}
	input, ok := toolUse["input"]
	if !ok {
		t.Fatal("tool_use block has no \"input\" key at all — Anthropic requires it even for a no-argument call")
	}
	inputObj, ok := input.(map[string]any)
	if !ok || len(inputObj) != 0 {
		t.Errorf("tool_use.input = %#v, want an empty JSON object {}", input)
	}
}
