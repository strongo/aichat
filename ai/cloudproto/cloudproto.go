// Package cloudproto is the small, product-neutral wire protocol between
// clients and an AI cloud boundary. It is OpenAI-inspired and event-oriented
// but deliberately NOT a provider's protocol: upstream providers (and any
// gateway such as Cloudflare AI Gateway behind the boundary) can change
// without changing this contract. The cloud boundary's base URL is supplied
// by the product (see ai/cloud.Config.BaseURL / ai/aiconfig); this package
// has no default of its own.
//
//	POST {base}ai/chat      body: ai.ChatRequest     → text/event-stream of ai.Event
//	POST {base}ai/decision  body: decision.Request   → application/json DecisionResponse
//	GET  {base}ai/usage     → application/json UsageResponse
//
// {base} is the API base URL including its version prefix, e.g.
// https://api.example.com/v0/. Requests carry the product's normal bearer
// authentication and the X-AI-Product header.
//
// SSE framing: each ai.Event is sent as
//
//	event: <ai.Event.Type>
//	data: <ai.Event as single-line JSON>
//	<blank line>
//
// Unknown event names must be ignored by clients. The stream ends after
// response.completed or a fatal error event (see ai.LLMProvider's
// fatal-error contract, which ReadEvents follows: an EventError frame is
// always fatal on this wire and ReadEvents stops after yielding it).
// Errors before streaming starts are ordinary HTTP errors with an
// ErrorResponse JSON body.
package cloudproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/internal/sse"
)

const (
	PathChat        = "ai/chat"
	PathDecision    = "ai/decision"
	PathUsage       = "ai/usage"
	PathInteraction = "ai/interactions"

	HeaderProduct  = "X-AI-Product"
	ContentTypeSSE = "text/event-stream"
)

// DecisionResponse is the body of POST ai/decision.
type DecisionResponse struct {
	Decided  bool              `json:"decided"`
	Decision decision.Decision `json:"decision,omitzero"`
	Model    string            `json:"model,omitempty"`
	Usage    *ai.Usage         `json:"usage,omitempty"`
}

// UsageResponse is the body of GET ai/usage.
type UsageResponse struct {
	Product   string        `json:"product"`
	Allowance *ai.Allowance `json:"allowance,omitempty"`
}

// ErrorResponse is the JSON body of a non-2xx response.
type ErrorResponse struct {
	Error ai.Error `json:"error"`
}

// WriteEvent writes one event in SSE framing. Callers flush after each call.
func WriteEvent(w io.Writer, ev ai.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	return err
}

// ReadEvents parses an SSE stream of ai.Event. It yields events as soon as
// each frame completes (no buffering of the whole response), skips comments
// and unknown event names, and stops after response.completed or after a
// fatal error.
//
// On this wire, an EventError frame is always fatal: ReadEvents yields it as
// the fatal pair (ev, err) with err a non-nil *ai.Error built from
// ev.Error, per the ai.LLMProvider contract, and returns immediately
// afterward. A transport EOF that arrives before either response.completed
// or an error frame was seen is itself a fatal truncation and is reported
// the same way.
func ReadEvents(r io.Reader) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		sc.Split(sse.ScanLines)
		var name string
		var data strings.Builder
		sawTerminal := false
		flush := func() (stop bool) {
			defer func() { name = ""; data.Reset() }()
			if data.Len() == 0 {
				return false
			}
			var ev ai.Event
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				sawTerminal = true
				aiErr := &ai.Error{Code: ai.ErrCodeUpstream, Message: fmt.Sprintf("cloudproto: bad event %q: %v", name, err)}
				yield(ai.Event{Type: ai.EventError, Error: aiErr}, aiErr)
				return true
			}
			if ev.Type == "" {
				ev.Type = ai.EventType(name)
			}
			if !known(ev.Type) {
				return false
			}
			if ev.Type == ai.EventError {
				sawTerminal = true
				aiErr := ev.Error
				if aiErr == nil {
					aiErr = &ai.Error{Code: ai.ErrCodeUpstream, Message: "cloudproto: error event with no detail"}
				}
				yield(ai.Event{Type: ai.EventError, Error: aiErr}, aiErr)
				return true
			}
			if !yield(ev, nil) {
				return true
			}
			if ev.Type == ai.EventCompleted {
				sawTerminal = true
				return true
			}
			return false
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if flush() {
					return
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err := sc.Err(); err != nil {
			aiErr := &ai.Error{Code: ai.ErrCodeUpstream, Message: fmt.Sprintf("cloudproto: transport error: %v", err)}
			yield(ai.Event{Type: ai.EventError, Error: aiErr}, aiErr)
			return
		}
		if flush() {
			return
		}
		if !sawTerminal {
			aiErr := &ai.Error{Code: ai.ErrCodeUpstream, Message: "cloudproto: stream truncated (no response.completed)"}
			yield(ai.Event{Type: ai.EventError, Error: aiErr}, aiErr)
		}
	}
}

func known(t ai.EventType) bool {
	switch t {
	case ai.EventStarted, ai.EventTextDelta, ai.EventStructured, ai.EventUsage, ai.EventError, ai.EventCompleted,
		ai.EventToolCall, ai.EventToolResult:
		return true
	}
	return false
}
