// Package agent implements a tool-calling agent loop over ai.LLMProvider: it
// streams a model's response, executes any tool calls the model asked for
// via caller-supplied Handlers, feeds the results back, and repeats until
// the model stops calling tools or a configured limit is hit.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
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
type Loop struct {
	Provider     ai.LLMProvider
	Handlers     map[string]Handler
	MaxSteps     int
	MaxToolCalls int

	mu         sync.Mutex
	transcript []ai.Message
}

// Name implements ai.LLMProvider.
func (l *Loop) Name() string { return "agent" }

// Stream implements ai.LLMProvider by delegating to Run.
func (l *Loop) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return l.Run(ctx, req)
}

// Messages returns the full transcript (including tool-call and tool-result
// messages) of the last completed Run, for session persistence. It is safe
// to call once Run's iterator has stopped yielding.
func (l *Loop) Messages() []ai.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ai.Message(nil), l.transcript...)
}

// Run streams every step's events in order (text deltas, tool.call,
// tool.result, usage) and ends with exactly one of:
//
//   - a final ai.EventCompleted carrying the SUMMED usage across every step, or
//   - a fatal error pair, per the ai.LLMProvider contract (an exceeded
//     MaxSteps/MaxToolCalls limit is reported as ai.Error{Code: "limit"}).
func (l *Loop) Run(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
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

		saveTranscript := func() {
			l.mu.Lock()
			l.transcript = append([]ai.Message(nil), messages...)
			l.mu.Unlock()
		}
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

			var stepCalls []ai.ToolCall
			var stepUsage *ai.Usage
			var stepProviderState json.RawMessage
			stopReason := ""
			fatalErr := (*ai.Error)(nil)

			for ev, err := range l.Provider.Stream(ctx, stepReq) {
				if err != nil {
					var aiErr *ai.Error
					if e, ok := err.(*ai.Error); ok {
						aiErr = e
					} else {
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

			if len(stepCalls) == 0 {
				// No tool calls this step: the run is done.
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

			// Append the assistant's tool-call message. ProviderState (e.g.
			// ai/anthropic's thinking/redacted_thinking blocks with
			// signatures) rides along unmodified so a later step that
			// re-sends this message satisfies the provider's replay
			// requirement — see ai.Message.ProviderState.
			messages = append(messages, ai.Message{
				Role:          ai.RoleAssistant,
				ToolCalls:     append([]ai.ToolCall(nil), stepCalls...),
				ProviderState: stepProviderState,
			})

			results := make([]ai.ToolResult, 0, len(stepCalls))
			for _, call := range stepCalls {
				toolCallsSoFar++
				if toolCallsSoFar > maxToolCalls {
					saveTranscript()
					yieldFatal(yield, &ai.Error{Code: "limit", Message: fmt.Sprintf("agent: exceeded MaxToolCalls (%d)", maxToolCalls)})
					return
				}
				if ctx.Err() != nil {
					saveTranscript()
					yieldCanceled(yield, ctx)
					return
				}
				result := l.execute(ctx, call)
				results = append(results, result)
				if !yield(ai.Event{Type: ai.EventToolResult, ToolResult: &result}, nil) {
					return
				}
			}
			messages = append(messages, ai.Message{Role: ai.RoleTool, ToolResults: results})
			saveTranscript()
		}
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
func (l *Loop) execute(ctx context.Context, call ai.ToolCall) (result ai.ToolResult) {
	result.CallID = call.ID
	h, ok := l.Handlers[call.Name]
	if !ok {
		result.IsError = true
		result.Content = fmt.Sprintf("no handler registered for tool %q", call.Name)
		return result
	}
	defer func() {
		if r := recover(); r != nil {
			result = ai.ToolResult{CallID: call.ID, IsError: true, Content: fmt.Sprintf("tool %q panicked: %v", call.Name, r)}
		}
	}()
	res, err := h(ctx, call)
	if err != nil {
		return ai.ToolResult{CallID: call.ID, IsError: true, Content: err.Error()}
	}
	if res.CallID == "" {
		res.CallID = call.ID
	}
	return res
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
