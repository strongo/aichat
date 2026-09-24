package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
)

// fakeProvider replays a scripted sequence of steps; each call to Stream
// consumes the next step and yields its events, honouring ctx cancellation.
type fakeProvider struct {
	steps [][]ai.Event
	calls int
	// reqs records each ChatRequest.Messages it was called with, so tests can
	// assert the transcript fed back between steps.
	reqs [][]ai.Message
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		f.reqs = append(f.reqs, append([]ai.Message(nil), req.Messages...))
		if f.calls >= len(f.steps) {
			yield(ai.Event{Type: ai.EventError, Error: &ai.Error{Code: ai.ErrCodeUpstream, Message: "no more scripted steps"}}, &ai.Error{Code: ai.ErrCodeUpstream})
			return
		}
		step := f.steps[f.calls]
		f.calls++
		for i, ev := range step {
			if ctx.Err() != nil {
				e := &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()}
				yield(ai.Event{Type: ai.EventError, Error: e}, e)
				return
			}
			// Only the LAST event of a scripted step is fatal when it is an
			// EventError; an EventError anywhere else in the middle is the
			// non-fatal kind (nil Go error), matching the LLMProvider
			// contract this fake replays.
			if ev.Type == ai.EventError && ev.Error != nil && i == len(step)-1 {
				yield(ev, ev.Error)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func toolCallStep(text string, calls ...ai.ToolCall) []ai.Event {
	evs := []ai.Event{{Type: ai.EventStarted, Provider: "fake"}}
	if text != "" {
		evs = append(evs, ai.Event{Type: ai.EventTextDelta, Text: text})
	}
	for i := range calls {
		c := calls[i]
		evs = append(evs, ai.Event{Type: ai.EventToolCall, ToolCall: &c})
	}
	usage := &ai.Usage{InputTokens: 10, OutputTokens: 5}
	stop := ai.StopReasonEnd
	if len(calls) > 0 {
		stop = ai.StopReasonToolCalls
	}
	evs = append(evs, ai.Event{Type: ai.EventUsage, Usage: usage}, ai.Event{Type: ai.EventCompleted, Usage: usage, StopReason: stop})
	return evs
}

func drain(t *testing.T, seq iter.Seq2[ai.Event, error]) (events []ai.Event, fatal error) {
	t.Helper()
	for ev, err := range seq {
		events = append(events, ev)
		if err != nil {
			fatal = err
			return events, fatal
		}
	}
	return events, nil
}

func TestLoop_TwoStepToolUse(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("let me check", ai.ToolCall{ID: "call_1", Name: "run_dtql", Arguments: json.RawMessage(`{"sql":"select 1"}`)}),
		toolCallStep("the answer is 1"),
	}}
	var handlerCalls int
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"run_dtql": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				handlerCalls++
				if call.ID != "call_1" {
					t.Errorf("call.ID = %q", call.ID)
				}
				return ai.ToolResult{CallID: call.ID, Content: "1 row"}, nil
			},
		},
	}

	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "how many?"}},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if handlerCalls != 1 {
		t.Fatalf("handlerCalls = %d, want 1", handlerCalls)
	}
	if provider.calls != 2 {
		t.Fatalf("provider.calls = %d, want 2", provider.calls)
	}

	var toolCallEvents, toolResultEvents, completedEvents int
	var finalUsage *ai.Usage
	for _, ev := range events {
		switch ev.Type {
		case ai.EventToolCall:
			toolCallEvents++
		case ai.EventToolResult:
			toolResultEvents++
			if ev.ToolResult == nil || ev.ToolResult.Content != "1 row" {
				t.Errorf("tool result event = %+v", ev.ToolResult)
			}
		case ai.EventCompleted:
			completedEvents++
			finalUsage = ev.Usage
		}
	}
	if toolCallEvents != 1 || toolResultEvents != 1 {
		t.Errorf("toolCallEvents=%d toolResultEvents=%d, want 1/1", toolCallEvents, toolResultEvents)
	}
	if completedEvents != 1 {
		t.Fatalf("completedEvents = %d, want exactly 1 (ONE final EventCompleted)", completedEvents)
	}
	// Usage summed across both steps: 10+10 input, 5+5 output.
	if finalUsage == nil || finalUsage.InputTokens != 20 || finalUsage.OutputTokens != 10 {
		t.Errorf("final usage = %+v, want summed 20/10", finalUsage)
	}

	// Messages() exposes the full transcript, including the tool-call and
	// tool-result messages appended between steps.
	transcript := loop.Messages()
	if len(transcript) != 3 {
		t.Fatalf("Messages() len = %d, want 3 (user, assistant tool_calls, tool result): %+v", len(transcript), transcript)
	}
	if transcript[1].Role != ai.RoleAssistant || len(transcript[1].ToolCalls) != 1 {
		t.Errorf("transcript[1] = %+v", transcript[1])
	}
	if transcript[2].Role != ai.RoleTool || len(transcript[2].ToolResults) != 1 {
		t.Errorf("transcript[2] = %+v", transcript[2])
	}

	// Loop also implements ai.LLMProvider.
	var _ ai.LLMProvider = loop
	if loop.Name() != "agent" {
		t.Errorf("Name() = %q, want agent", loop.Name())
	}
}

func TestLoop_ExceedsMaxSteps(t *testing.T) {
	// Every step calls a tool, so the loop never naturally stops -- MaxSteps
	// must cut it off with a fatal "limit" error.
	steps := make([][]ai.Event, 5)
	for i := range steps {
		steps[i] = toolCallStep("", ai.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "noop"})
	}
	provider := &fakeProvider{steps: steps}
	loop := &Loop{
		Provider: provider,
		MaxSteps: 2,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err == nil {
		t.Fatal("expected a fatal limit error")
	}
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != "limit" {
		t.Errorf("err = %v, want ai.Error{Code: \"limit\"}", err)
	}
}

func TestLoop_ExceedsMaxToolCalls(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("",
			ai.ToolCall{ID: "call_1", Name: "noop"},
			ai.ToolCall{ID: "call_2", Name: "noop"},
			ai.ToolCall{ID: "call_3", Name: "noop"},
		),
	}}
	loop := &Loop{
		Provider:     provider,
		MaxToolCalls: 2,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != "limit" {
		t.Errorf("err = %v, want ai.Error{Code: \"limit\"} for exceeded MaxToolCalls", err)
	}
}

func TestLoop_HandlerErrorBecomesIsErrorResultNotFatal(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "flaky"}),
		toolCallStep("recovered"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"flaky": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{}, errors.New("boom: infra down")
			},
		},
	}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v, want the loop to continue past a Handler error as an IsError result", err)
	}
	var sawErrorResult bool
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil && ev.ToolResult.IsError {
			sawErrorResult = true
			if ev.ToolResult.CallID != "call_1" {
				t.Errorf("CallID = %q", ev.ToolResult.CallID)
			}
		}
	}
	if !sawErrorResult {
		t.Error("expected an IsError ToolResult event for the failing handler")
	}
}

func TestLoop_HandlerPanicRecovered(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "panicky"}),
		toolCallStep("ok"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"panicky": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				panic("kaboom")
			},
		},
	}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v, want a recovered panic to become an IsError result, not a fatal Run failure", err)
	}
	var sawErrorResult bool
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil && ev.ToolResult.IsError {
			sawErrorResult = true
		}
	}
	if !sawErrorResult {
		t.Error("expected an IsError ToolResult event for the panicking handler")
	}
}

func TestLoop_MissingHandlerBecomesIsErrorResult(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "unregistered"}),
		toolCallStep("ok"),
	}}
	loop := &Loop{Provider: provider}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawErrorResult bool
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil && ev.ToolResult.IsError {
			sawErrorResult = true
		}
	}
	if !sawErrorResult {
		t.Error("expected an IsError ToolResult event for the unregistered tool")
	}
}

func TestLoop_ContextCancelYieldsCanceledFatalPair(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "slow"}),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"slow": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				cancel()
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Errorf("err = %v, want ai.Error{Code: ai.ErrCodeCanceled}", err)
	}
}

func TestLoop_NoToolCallsCompletesImmediately(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{toolCallStep("hi there")}}
	loop := &Loop{Provider: provider}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1", provider.calls)
	}
	last := events[len(events)-1]
	if last.Type != ai.EventCompleted || last.StopReason != ai.StopReasonEnd {
		t.Errorf("last event = %+v, want Completed/end", last)
	}
}

// TestLoop_ParallelCallsExecutedSequentiallyInOrder verifies the pinned
// contract that parallel tool calls in one step run one after another, in
// the order the model returned them (not concurrently).
func TestLoop_ParallelCallsExecutedSequentiallyInOrder(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("",
			ai.ToolCall{ID: "call_1", Name: "record"},
			ai.ToolCall{ID: "call_2", Name: "record"},
			ai.ToolCall{ID: "call_3", Name: "record"},
		),
		toolCallStep("done"),
	}}
	var order []string
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"record": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				time.Sleep(time.Millisecond)
				order = append(order, call.ID)
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"call_1", "call_2", "call_3"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestLoop_StreamDelegatesToRun(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{toolCallStep("hi")}}
	loop := &Loop{Provider: provider}
	events, err := drain(t, loop.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events from Stream")
	}
}

func TestLoop_NonFatalErrorEventPassesThrough(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		{
			{Type: ai.EventStarted, Provider: "fake"},
			{Type: ai.EventError, Error: &ai.Error{Code: ai.ErrCodeUpstream, Message: "transient diagnostic"}},
			{Type: ai.EventCompleted, StopReason: ai.StopReasonEnd},
		},
	}}
	loop := &Loop{Provider: provider}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Run: %v, want the non-fatal EventError (nil Go error) to pass through, not abort", err)
	}
	var sawNonFatal bool
	for _, ev := range events {
		if ev.Type == ai.EventError {
			sawNonFatal = true
		}
	}
	if !sawNonFatal {
		t.Error("expected the non-fatal error event to be forwarded")
	}
}
