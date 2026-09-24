// Package agent implements a tool-calling agent loop over ai.LLMProvider: it
// streams a model's response, executes any tool calls the model asked for
// via caller-supplied Handlers, feeds the results back, and repeats until
// the model stops calling tools or a configured limit is hit.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"

	"github.com/strongo/aichat/ai"
)

// defaults for Loop.MaxSteps / Loop.MaxToolCalls when left zero.
const (
	defaultMaxSteps     = 8
	defaultMaxToolCalls = 16
)

// Handler executes one tool call. A non-nil error is an INFRASTRUCTURE
// failure (network down, panic-equivalent, programmer error) and aborts the
// whole Run with a fatal error; a tool-level failure (bad arguments, the
// underlying operation failed in an expected way) is reported by returning
// an ai.ToolResult with IsError true and a nil error, so the model can see
// and react to it.
type Handler func(ctx context.Context, call ai.ToolCall) (ai.ToolResult, error)

// Loop drives a tool-calling agent over Provider. The zero value is not
// ready to use — Provider is required; MaxSteps/MaxToolCalls default to 8/16
// when left zero.
//
// Loop itself holds NO mutable run state (r1 review, M9: it used to embed a
// sync.Mutex + the last run's transcript directly, which made a Loop value
// unsafe to copy — go vet's copylocks check, and worse, two Loop values
// produced by copying one after a Run would share/race on that internal
// state). A Loop value is now freely copyable and reusable across
// concurrent Runs: each Run/RunWithTranscript call keeps its own transcript
// entirely in a closure-local variable.
type Loop struct {
	Provider     ai.LLMProvider
	Handlers     map[string]Handler
	MaxSteps     int
	MaxToolCalls int
}

// Name implements ai.LLMProvider.
func (l Loop) Name() string { return "agent" }

// Stream implements ai.LLMProvider by delegating to Run.
func (l Loop) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return l.Run(ctx, req)
}

// Run streams every step's events in order (text deltas, tool.call,
// tool.result, usage) and ends with exactly one of:
//
//   - a final ai.EventCompleted carrying the SUMMED usage across every step, or
//   - a fatal error pair, per the ai.LLMProvider contract (an exceeded
//     MaxSteps/MaxToolCalls limit is reported as ai.Error{Code: "limit"}).
//
// Run alone does not expose the transcript it built (see RunWithTranscript
// for that); use it when only the streamed events matter, e.g. Loop used
// purely as an ai.LLMProvider.
func (l Loop) Run(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	seq, _ := l.RunWithTranscript(ctx, req)
	return seq
}

// RunWithTranscript is Run plus an accessor for the transcript (including
// the tool-call/tool-result messages appended between steps, and the final
// step's own assistant reply) this specific call is building, for session
// persistence. The accessor is safe to call at any time, including
// concurrently with draining the returned iterator (e.g. to show a live
// "assistant is thinking" partial transcript) — it always returns a
// defensive copy of whatever has been appended so far.
func (l Loop) RunWithTranscript(ctx context.Context, req ai.ChatRequest) (iter.Seq2[ai.Event, error], func() []ai.Message) {
	var mu sync.Mutex
	var transcript []ai.Message
	save := func(messages []ai.Message) {
		mu.Lock()
		transcript = append([]ai.Message(nil), messages...)
		mu.Unlock()
	}
	accessor := func() []ai.Message {
		mu.Lock()
		defer mu.Unlock()
		return append([]ai.Message(nil), transcript...)
	}
	seq := func(yield func(ai.Event, error) bool) {
		l.run(ctx, req, yield, save)
	}
	return seq, accessor
}

// run is Run/RunWithTranscript's shared body. save is called with the
// current message list at every point Run itself would previously have
// written l.transcript (start, each step boundary, and every early return).
func (l Loop) run(ctx context.Context, req ai.ChatRequest, yield func(ai.Event, error) bool, save func([]ai.Message)) {
	maxSteps := l.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	maxToolCalls := l.MaxToolCalls
	if maxToolCalls <= 0 {
		maxToolCalls = defaultMaxToolCalls
	}

	messages := append([]ai.Message(nil), req.Messages...)
	var summed ai.Usage
	haveUsage := false
	toolCallsSoFar := 0
	startedYielded := false

	saveTranscript := func() { save(messages) }
	saveTranscript()

	for step := 1; ; step++ {
		if ctx.Err() != nil {
			saveTranscript()
			yieldCanceled(yield, ctx)
			return
		}
		if step > maxSteps {
			saveTranscript()
			yieldFatal(yield, &ai.Error{Code: "limit", Message: fmt.Sprintf("agent: exceeded MaxSteps (%d)", maxSteps)})
			return
		}

		stepReq := req
		stepReq.Messages = messages
		if step > 1 {
			// M2 (r1 review): a forced ToolChoice ("required", or a
			// specific tool name) only applies to the FIRST step. Carrying
			// it forward would force the model to keep calling tools
			// forever, defeating the loop's natural termination (a step
			// with no tool calls).
			stepReq.ToolChoice = ai.ToolChoiceAuto
		}

		var stepCalls []ai.ToolCall
		var stepUsage *ai.Usage
		var stepProviderState json.RawMessage
		var stepText strings.Builder
		stopReason := ""
		fatalErr := (*ai.Error)(nil)

		for ev, err := range l.Provider.Stream(ctx, stepReq) {
			if err != nil {
				var aiErr *ai.Error
				if !errors.As(err, &aiErr) {
					aiErr = &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error()}
				}
				fatalErr = aiErr
				break
			}
			switch ev.Type {
			case ai.EventStarted:
				if startedYielded {
					continue
				}
				startedYielded = true
				if !yield(ev, nil) {
					return
				}
			case ai.EventTextDelta:
				stepText.WriteString(ev.Text)
				if !yield(ev, nil) {
					return
				}
			case ai.EventToolCall:
				if ev.ToolCall != nil {
					stepCalls = append(stepCalls, *ev.ToolCall)
				}
				if !yield(ev, nil) {
					return
				}
			case ai.EventUsage:
				if ev.Usage != nil {
					stepUsage = ev.Usage
				}
				if !yield(ev, nil) {
					return
				}
			case ai.EventCompleted:
				if ev.Usage != nil {
					stepUsage = ev.Usage
				}
				stopReason = ev.StopReason
				stepProviderState = ev.ProviderState
				// Swallow the per-step Completed: Run yields exactly one,
				// at the very end.
			case ai.EventError:
				// Non-fatal EventError (fatal ones arrive via the iterator's
				// err return, handled above): pass through.
				if !yield(ev, nil) {
					return
				}
			default:
				if !yield(ev, nil) {
					return
				}
			}
		}

		if fatalErr != nil {
			saveTranscript()
			yieldFatal(yield, fatalErr)
			return
		}
		if stepUsage != nil {
			summed = sumUsage(summed, *stepUsage)
			haveUsage = true
		}

		if len(stepCalls) > 0 && stopReason == ai.StopReasonLength {
			// M3 (r1 review): the response was cut off mid-generation (the
			// provider's own "length"/"max_tokens" stop reason) while it
			// still had at least one tool call open -- that call's
			// arguments may be a truncated, incomplete JSON fragment.
			// Running a Handler on possibly-garbage arguments is worse than
			// failing loudly, so this is fatal rather than attempted; the
			// transcript is untouched (nothing from this step was appended
			// yet), so the caller's last successfully saved state is still
			// exactly what RunWithTranscript's accessor returns.
			saveTranscript()
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "agent: response truncated (stop reason \"length\") with an in-progress tool call; arguments may be incomplete"})
			return
		}

		if len(stepCalls) == 0 {
			// No tool calls this step: the run is done. B3 (r1 review):
			// this final turn's own text/ProviderState was previously
			// dropped entirely -- Messages() ended at the last tool
			// result, silently discarding the model's actual answer.
			// Append it as an ordinary assistant message before saving.
			messages = append(messages, ai.Message{
				Role:          ai.RoleAssistant,
				Text:          stepText.String(),
				ProviderState: stepProviderState,
			})
			saveTranscript()
			var usagePtr *ai.Usage
			if haveUsage {
				u := summed
				usagePtr = &u
			}
			finalStop := stopReason
			if finalStop == "" {
				finalStop = ai.StopReasonEnd
			}
			yield(ai.Event{Type: ai.EventCompleted, Usage: usagePtr, StopReason: finalStop}, nil)
			return
		}

		// Append the assistant's tool-call message, including any text
		// the model wrote alongside the call (e.g. "let me check that").
		// ProviderState (e.g. ai/anthropic's thinking/redacted_thinking
		// blocks with signatures) rides along unmodified so a later step
		// that re-sends this message satisfies the provider's replay
		// requirement — see ai.Message.ProviderState.
		messages = append(messages, ai.Message{
			Role:          ai.RoleAssistant,
			Text:          stepText.String(),
			ToolCalls:     append([]ai.ToolCall(nil), stepCalls...),
			ProviderState: stepProviderState,
		})

		results := make([]ai.ToolResult, 0, len(stepCalls))
		for i, call := range stepCalls {
			toolCallsSoFar++
			var abortErr *ai.Error
			switch {
			case toolCallsSoFar > maxToolCalls:
				abortErr = &ai.Error{Code: "limit", Message: fmt.Sprintf("agent: exceeded MaxToolCalls (%d)", maxToolCalls)}
			case ctx.Err() != nil:
				abortErr = &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()}
			}
			var handlerErr error
			if abortErr == nil {
				result, err := l.execute(ctx, call)
				if err == nil {
					results = append(results, result)
					if !yield(ai.Event{Type: ai.EventToolResult, ToolResult: &result}, nil) {
						return
					}
					continue
				}
				// M4 (r1 review, per the pinned brief): a Handler error is
				// an infrastructure failure and ABORTS the Run.
				handlerErr = err
				abortErr = &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error()}
			}
			// M3 (r1 review): aborting mid tool-call loop (limit exceeded,
			// ctx cancelled, or a Handler infrastructure failure) must not
			// leave the assistant tool_calls message already appended to
			// messages with some calls unanswered -- every provider
			// requires each tool_use to have a matching tool_result before
			// the next turn, so an unanswered one makes the persisted
			// transcript invalid to replay. Synthesize an IsError result
			// for THIS call and every remaining one that never got a
			// chance to run, append them all, THEN report the fatal error.
			reason := "not executed: canceled"
			switch {
			case handlerErr != nil:
				reason = "not executed: " + handlerErr.Error()
			case abortErr.Code == "limit":
				reason = "not executed: limit"
			}
			for _, unanswered := range stepCalls[i:] {
				r := ai.ToolResult{CallID: unanswered.ID, IsError: true, Content: reason}
				results = append(results, r)
				if !yield(ai.Event{Type: ai.EventToolResult, ToolResult: &r}, nil) {
					return
				}
			}
			messages = append(messages, ai.Message{Role: ai.RoleTool, ToolResults: results})
			saveTranscript()
			yieldFatal(yield, abortErr)
			return
		}
		messages = append(messages, ai.Message{Role: ai.RoleTool, ToolResults: results})
		saveTranscript()
	}
}

// execute runs a single Handler, converting a missing handler, an
// infrastructure error, or a recovered panic into an IsError ToolResult so
// the run itself never aborts on a single tool's misbehaviour except via the
// documented "error = infrastructure failure (aborts)" contract, which this
// package honours by treating a returned error the same as a panic: both
// become an IsError result rather than a fatal Run failure, keeping the loop
// resilient to individual tool failures while still surfacing them to the
// model.
// execute runs call's Handler. Ruling (r1 review, M4 — the pinned brief's
// own words: "error = infrastructure failure (aborts); tool-level failures
// return IsError results"): a Handler returning a non-nil error is an
// INFRASTRUCTURE failure and ABORTS the whole Run (execute reports it via
// the returned error, which the caller turns into the fatal pair) — it is
// NOT converted to an IsError ToolResult. A missing handler, invalid-JSON
// arguments, or a recovered panic are, by contrast, TOOL-LEVEL failures and
// DO become IsError results without aborting.
func (l Loop) execute(ctx context.Context, call ai.ToolCall) (result ai.ToolResult, abort error) {
	result.CallID = call.ID
	// Reject invalid-JSON arguments before ever reaching a Handler -- a
	// Handler expects call.Arguments to already be valid JSON (it's typed
	// json.RawMessage), and validating it here once means every Handler
	// doesn't have to. Empty Arguments is fine (a no-argument call);
	// anything else must be syntactically valid JSON.
	if len(call.Arguments) > 0 && !json.Valid(call.Arguments) {
		result.IsError = true
		result.Content = fmt.Sprintf("tool %q: invalid JSON arguments", call.Name)
		return result, nil
	}
	h, ok := l.Handlers[call.Name]
	if !ok {
		result.IsError = true
		result.Content = fmt.Sprintf("no handler registered for tool %q", call.Name)
		return result, nil
	}
	defer func() {
		if r := recover(); r != nil {
			result = ai.ToolResult{CallID: call.ID, IsError: true, Content: fmt.Sprintf("tool %q panicked: %v", call.Name, r)}
			abort = nil
		}
	}()
	res, err := h(ctx, call)
	if err != nil {
		return ai.ToolResult{}, fmt.Errorf("tool %q: %w", call.Name, err)
	}
	if res.CallID == "" {
		res.CallID = call.ID
	}
	return res, nil
}

func sumUsage(a, b ai.Usage) ai.Usage {
	return ai.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		Allowance:        b.Allowance,
	}
}

func yieldFatal(yield func(ai.Event, error) bool, e *ai.Error) {
	yield(ai.Event{Type: ai.EventError, Error: e}, e)
}

func yieldCanceled(yield func(ai.Event, error) bool, ctx context.Context) {
	e := &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()}
	yieldFatal(yield, e)
}
