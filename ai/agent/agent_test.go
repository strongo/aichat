package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
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

	seq, transcriptFn := loop.RunWithTranscript(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "how many?"}},
	})
	events, err := drain(t, seq)
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

	// RunWithTranscript's accessor exposes the full transcript, including
	// the tool-call and tool-result messages appended between steps, AND
	// the final step's own assistant reply (B3, r1 review: this used to be
	// silently dropped).
	transcript := transcriptFn()
	if len(transcript) != 4 {
		t.Fatalf("Messages() len = %d, want 4 (user, assistant tool_calls, tool result, final assistant): %+v", len(transcript), transcript)
	}
	if transcript[1].Role != ai.RoleAssistant || len(transcript[1].ToolCalls) != 1 || transcript[1].Text != "let me check" {
		t.Errorf("transcript[1] = %+v, want the step-1 narrative text alongside its tool call", transcript[1])
	}
	if transcript[2].Role != ai.RoleTool || len(transcript[2].ToolResults) != 1 {
		t.Errorf("transcript[2] = %+v", transcript[2])
	}
	if transcript[3].Role != ai.RoleAssistant || transcript[3].Text != "the answer is 1" || len(transcript[3].ToolCalls) != 0 {
		t.Errorf("transcript[3] = %+v, want the final step's own text-only assistant message", transcript[3])
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

// TestLoop_HandlerErrorAbortsWithFatalPair covers the coordinator's r1 ruling
// on M4: a Handler returning a non-nil error is an infrastructure failure and
// MUST abort the Run as a fatal error pair, exactly like exceeding
// MaxSteps/MaxToolCalls or a canceled context — it is NOT downgraded to an
// IsError ToolResult. Only tool-level failures (missing handler, invalid JSON
// arguments, a recovered panic) become IsError results that the loop feeds
// back to the model and continues past.
func TestLoop_HandlerErrorAbortsWithFatalPair(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("",
			ai.ToolCall{ID: "call_1", Name: "flaky"},
			ai.ToolCall{ID: "call_2", Name: "flaky"},
		),
		toolCallStep("unreachable"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"flaky": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{}, errors.New("boom: infra down")
			},
		},
	}
	seq, transcriptFn := loop.RunWithTranscript(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}})
	events, err := drain(t, seq)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) {
		t.Fatalf("err = %v, want a fatal *ai.Error (Handler errors abort the run per M4)", err)
	}
	if !strings.Contains(aiErr.Message, "boom: infra down") {
		t.Errorf("aiErr.Message = %q, want it to surface the Handler error", aiErr.Message)
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1: the loop must not take a second step after a fatal Handler error", provider.calls)
	}

	// The failing call and every call left unanswered in the same step must
	// still be synthesized as IsError results so the persisted transcript is
	// a valid, replayable conversation.
	var sawErrorResult int
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil {
			if !ev.ToolResult.IsError {
				t.Errorf("ToolResult %+v, want IsError: no handler runs after the fatal error", ev.ToolResult)
			}
			sawErrorResult++
		}
	}
	if sawErrorResult != 2 {
		t.Errorf("sawErrorResult = %d, want 2 (call_1 failed, call_2 never executed)", sawErrorResult)
	}

	transcript := transcriptFn()
	var toolMsg *ai.Message
	for i := range transcript {
		if transcript[i].Role == ai.RoleTool {
			toolMsg = &transcript[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no RoleTool message in the persisted transcript")
	}
	if len(toolMsg.ToolResults) != 2 {
		t.Fatalf("ToolResults = %+v, want 2 (every tool_use answered, even the one never executed)", toolMsg.ToolResults)
	}
	if toolMsg.ToolResults[0].CallID != "call_1" || !toolMsg.ToolResults[0].IsError {
		t.Errorf("call_1 result = %+v, want IsError (the failing handler)", toolMsg.ToolResults[0])
	}
	if toolMsg.ToolResults[1].CallID != "call_2" || !toolMsg.ToolResults[1].IsError {
		t.Errorf("call_2 result = %+v, want IsError (never executed)", toolMsg.ToolResults[1])
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

// TestLoop_EmptyFinalTurnNotAppendedToTranscript covers r2's N1: a final
// step with no text, no tool calls and no ProviderState (e.g. a refusal, or
// an empty end_turn) must not be appended to the transcript at all -- an
// empty assistant message is wire noise the next adapter would otherwise
// have to reject or pad around.
func TestLoop_EmptyFinalTurnNotAppendedToTranscript(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{toolCallStep("")}}
	loop := &Loop{Provider: provider}
	seq, transcriptFn := loop.RunWithTranscript(context.Background(), ai.ChatRequest{
		Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}},
	})
	_, err := drain(t, seq)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	transcript := transcriptFn()
	if len(transcript) != 1 {
		t.Fatalf("transcript = %+v, want just the original user message (no empty assistant turn appended)", transcript)
	}
	if transcript[0].Role != ai.RoleUser {
		t.Errorf("transcript[0].Role = %v, want RoleUser", transcript[0].Role)
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

func TestLoop_ToolChoiceResetToAutoAfterStepOne(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "noop"}),
		toolCallStep("done"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{
		Messages:   []ai.Message{{Role: ai.RoleUser, Text: "go"}},
		ToolChoice: "required",
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.reqs) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(provider.reqs))
	}
}

func TestLoop_ExceedsMaxToolCallsSynthesizesIsErrorForUnansweredCalls(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("",
			ai.ToolCall{ID: "call_1", Name: "noop"},
			ai.ToolCall{ID: "call_2", Name: "noop"},
			ai.ToolCall{ID: "call_3", Name: "noop"},
		),
	}}
	loop := &Loop{
		Provider:     provider,
		MaxToolCalls: 1,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	seq, transcriptFn := loop.RunWithTranscript(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}})
	_, err := drain(t, seq)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != "limit" {
		t.Fatalf("err = %v, want ai.Error{Code: \"limit\"}", err)
	}

	transcript := transcriptFn()
	var toolMsg *ai.Message
	for i := range transcript {
		if transcript[i].Role == ai.RoleTool {
			toolMsg = &transcript[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no RoleTool message in the persisted transcript")
	}
	if len(toolMsg.ToolResults) != 3 {
		t.Fatalf("ToolResults = %+v, want 3 (every tool_use answered, even the ones never executed)", toolMsg.ToolResults)
	}
	if toolMsg.ToolResults[0].CallID != "call_1" || toolMsg.ToolResults[0].IsError {
		t.Errorf("call_1 result = %+v, want the one call that ran under the limit, not an error", toolMsg.ToolResults[0])
	}
	for _, r := range toolMsg.ToolResults[1:] {
		if !r.IsError {
			t.Errorf("result %+v, want IsError (never executed: limit exceeded)", r)
		}
	}
}

func TestLoop_TruncatedResponseWithToolCallIsFatalNeverRunsHandler(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		{
			{Type: ai.EventStarted},
			{Type: ai.EventToolCall, ToolCall: &ai.ToolCall{ID: "call_1", Name: "noop", Arguments: json.RawMessage(`{"partial":`)}},
			{Type: ai.EventCompleted, StopReason: ai.StopReasonLength},
		},
	}}
	var handlerCalled bool
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				handlerCalled = true
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err == nil {
		t.Fatal("expected a fatal error for a truncated response with an in-progress tool call")
	}
	if handlerCalled {
		t.Error("Handler must never run on a possibly-truncated tool call's arguments")
	}
}

func TestLoop_InvalidJSONArgumentsBecomeIsErrorWithoutCallingHandler(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "noop", Arguments: json.RawMessage(`{not valid json`)}),
		toolCallStep("ok"),
	}}
	var handlerCalled bool
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				handlerCalled = true
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if handlerCalled {
		t.Error("Handler must never be called with invalid JSON arguments")
	}
	var sawErrorResult bool
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil && ev.ToolResult.IsError {
			sawErrorResult = true
		}
	}
	if !sawErrorResult {
		t.Error("expected an IsError result for the invalid-JSON call")
	}
}

// TestLoop_ValueSafeCopyableAndReusable is M9's regression test: a Loop
// value (no pointer) must be safely copyable and independently reusable —
// it holds no run-scoped mutable state of its own.
// plainErrorProvider yields a plain (non-*ai.Error) Go error, unlike
// fakeProvider which always wraps ev.Error (*ai.Error). Covers run()'s
// errors.As fallback: a non-*ai.Error from Provider.Stream must be wrapped as
// ai.Error{Code: ai.ErrCodeUpstream} before becoming the fatal error.
type plainErrorProvider struct{}

func (plainErrorProvider) Name() string { return "plain" }

func (plainErrorProvider) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		if !yield(ai.Event{Type: ai.EventStarted}, nil) {
			return
		}
		yield(ai.Event{Type: ai.EventError}, errors.New("boom: plain transport failure"))
	}
}

func TestLoop_NonAIErrorFromProviderWrappedAsUpstream(t *testing.T) {
	loop := &Loop{Provider: plainErrorProvider{}}
	_, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) {
		t.Fatalf("err = %v, want *ai.Error", err)
	}
	if aiErr.Code != ai.ErrCodeUpstream {
		t.Errorf("Code = %q, want %q", aiErr.Code, ai.ErrCodeUpstream)
	}
	if !strings.Contains(aiErr.Message, "boom: plain transport failure") {
		t.Errorf("Message = %q, want it to mention the underlying plain error", aiErr.Message)
	}
}

// TestLoop_ConsumerStopsIterationAtEachEventType covers every "if !yield(ev,
// nil) { return }" branch inside run()'s per-event switch: a consumer that
// stops draining (via `break` in a range-over-func loop, which the Go
// runtime turns into the iterator's yield returning false) must cause run()
// to return immediately at that exact point, regardless of which event type
// it was in the middle of yielding.
func TestLoop_ConsumerStopsIterationAtEachEventType(t *testing.T) {
	cases := []struct {
		name     string
		step     []ai.Event
		stopType ai.EventType
	}{
		{"Started", []ai.Event{{Type: ai.EventStarted}}, ai.EventStarted},
		{"TextDelta", []ai.Event{{Type: ai.EventStarted}, {Type: ai.EventTextDelta, Text: "x"}}, ai.EventTextDelta},
		{"ToolCall", []ai.Event{{Type: ai.EventStarted}, {Type: ai.EventToolCall, ToolCall: &ai.ToolCall{ID: "call_1", Name: "noop"}}}, ai.EventToolCall},
		{"Usage", []ai.Event{{Type: ai.EventStarted}, {Type: ai.EventUsage, Usage: &ai.Usage{InputTokens: 1}}}, ai.EventUsage},
		// Error not in the last position -> fakeProvider treats it as
		// non-fatal (yields it with a nil Go error), matching run()'s
		// EventError case (fatal ones arrive via the iterator's err return
		// instead, handled by a separate branch). Stop AT the Error event
		// itself, not the trailing Completed, to hit its own "if !yield {
		// return }" branch.
		{"NonFatalError", []ai.Event{{Type: ai.EventStarted}, {Type: ai.EventError, Error: &ai.Error{Code: ai.ErrCodeUpstream, Message: "diag"}}, {Type: ai.EventCompleted}}, ai.EventError},
		// EventStructured has no explicit case in run()'s switch -> default.
		{"DefaultUnknownType", []ai.Event{{Type: ai.EventStarted}, {Type: ai.EventStructured, Structured: json.RawMessage(`{}`)}}, ai.EventStructured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{steps: [][]ai.Event{tc.step}}
			loop := &Loop{Provider: provider}
			var got []ai.Event
			for ev, _ := range loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
				got = append(got, ev)
				if ev.Type == tc.stopType {
					break
				}
			}
			if len(got) == 0 || got[len(got)-1].Type != tc.stopType {
				t.Fatalf("got = %+v, want the last event to be %v", got, tc.stopType)
			}
		})
	}
}

// TestLoop_ConsumerStopsAtFirstToolResultEvent covers the regular (non-abort)
// "if !yield(ai.Event{Type: EventToolResult...}) { return }" branch: a
// consumer stopping right at the first successful tool result must halt the
// run before the loop ever takes a second step.
func TestLoop_ConsumerStopsAtFirstToolResultEvent(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "noop"}),
		toolCallStep("unreachable"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	var sawToolResult bool
	for ev := range loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}) {
		if ev.Type == ai.EventToolResult {
			sawToolResult = true
			break
		}
	}
	if !sawToolResult {
		t.Fatal("expected to observe a ToolResult event before stopping")
	}
	if provider.calls != 1 {
		t.Errorf("provider.calls = %d, want 1: stopping mid-step must not take a second step", provider.calls)
	}
}

// TestLoop_YieldFalseDuringUnansweredSynthesisStopsRun covers the "for _,
// unanswered := range stepCalls[i:] { ...; if !yield(...) { return } }"
// branch specifically: a consumer that stops on the SECOND ToolResult event
// (the first synthesized IsError result for a call that never ran, once
// MaxToolCalls is exceeded) must halt mid-synthesis, before every remaining
// call gets its own synthesized result.
func TestLoop_YieldFalseDuringUnansweredSynthesisStopsRun(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("",
			ai.ToolCall{ID: "call_1", Name: "noop"},
			ai.ToolCall{ID: "call_2", Name: "noop"},
			ai.ToolCall{ID: "call_3", Name: "noop"},
		),
	}}
	loop := &Loop{
		Provider:     provider,
		MaxToolCalls: 1,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	var toolResults int
	for ev := range loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}) {
		if ev.Type == ai.EventToolResult {
			toolResults++
			if toolResults == 2 {
				break
			}
		}
	}
	if toolResults != 2 {
		t.Fatalf("toolResults = %d, want 2 (stopped mid-synthesis, after the 2nd)", toolResults)
	}
}

// TestLoop_CtxCanceledMidParallelCallsAbortsLaterCallBeforeExecute covers the
// "case ctx.Err() != nil: abortErr = ..." branch reached BEFORE calling
// execute for a later call in the SAME step, when an earlier call in that
// step canceled the context. Unlike
// TestLoop_ContextCancelYieldsCanceledFatalPair (a single call), this needs
// at least two calls in one step so the second call's abort check observes
// the cancellation the first call's Handler already performed.
func TestLoop_CtxCanceledMidParallelCallsAbortsLaterCallBeforeExecute(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "cancel_ctx"}, ai.ToolCall{ID: "call_2", Name: "noop"}),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var noopCalled bool
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"cancel_ctx": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				cancel()
				return ai.ToolResult{CallID: call.ID}, nil
			},
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				noopCalled = true
				return ai.ToolResult{CallID: call.ID}, nil
			},
		},
	}
	_, err := drain(t, loop.Run(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeCanceled}", err)
	}
	if noopCalled {
		t.Error("call_2's Handler must never run: ctx was already canceled before its abort check, ahead of execute()")
	}
}

// TestLoop_ExecuteFillsEmptyResultCallID covers execute()'s "if res.CallID ==
// \"\" { res.CallID = call.ID }" branch: a Handler that forgets to set
// ToolResult.CallID must have it filled in from the call automatically.
func TestLoop_ExecuteFillsEmptyResultCallID(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		toolCallStep("", ai.ToolCall{ID: "call_1", Name: "noop"}),
		toolCallStep("done"),
	}}
	loop := &Loop{
		Provider: provider,
		Handlers: map[string]Handler{
			"noop": func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error) {
				return ai.ToolResult{Content: "ok"}, nil // CallID deliberately left empty
			},
		},
	}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "go"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == ai.EventToolResult && ev.ToolResult != nil {
			found = true
			if ev.ToolResult.CallID != "call_1" {
				t.Errorf("CallID = %q, want call_1 filled in from the call", ev.ToolResult.CallID)
			}
		}
	}
	if !found {
		t.Fatal("no tool result event observed")
	}
}

// TestLoop_EmptyStopReasonDefaultsToEnd covers "if finalStop == \"\" {
// finalStop = ai.StopReasonEnd }": a final step whose EventCompleted left
// StopReason empty must still surface StopReasonEnd on the run's own final
// EventCompleted.
func TestLoop_EmptyStopReasonDefaultsToEnd(t *testing.T) {
	provider := &fakeProvider{steps: [][]ai.Event{
		{{Type: ai.EventStarted}, {Type: ai.EventCompleted}}, // StopReason left empty
	}}
	loop := &Loop{Provider: provider}
	events, err := drain(t, loop.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != ai.EventCompleted || last.StopReason != ai.StopReasonEnd {
		t.Errorf("last = %+v, want StopReason defaulted to ai.StopReasonEnd", last)
	}
}

func TestLoop_ValueSafeCopyableAndReusable(t *testing.T) {
	// Each copy gets its OWN fakeProvider (the fake itself is not
	// concurrency-safe, unlike Loop) so this isolates Loop's own
	// value-safety from the test double's.
	base := Loop{}
	copyA := base // struct copy -- would fail go vet's copylocks check if Loop still embedded a sync.Mutex
	copyA.Provider = &fakeProvider{steps: [][]ai.Event{toolCallStep("hi")}}
	copyB := base
	copyB.Provider = &fakeProvider{steps: [][]ai.Event{toolCallStep("hi")}}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	run := func(l Loop, i int) {
		defer wg.Done()
		_, err := drain(t, l.Run(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
		errs[i] = err
	}
	wg.Add(2)
	go run(copyA, 0)
	go run(copyB, 1)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
}
