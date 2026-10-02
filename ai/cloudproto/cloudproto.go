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
//	POST {base}ai/score     body: ScoreRequest       → application/json ScoreResponse
//	GET  {base}ai/usage     → application/json UsageResponse
//
// ai/score is additive within a version. A server that predates it answers 501
// (preferred) or 404/405, and a client then treats scoring as "not supported":
// the engine is skipped, a circuit breaker is left alone, and the finding is
// remembered for the client's lifetime (cloud.Client.ResetCapabilities forgets
// it) so the route is not probed on every call. Only an UNAMBIGUOUS signal counts
// as "route not implemented": a 501, or a 404/405 whose body is not an
// ErrorResponse AND where GET ai/usage shows the base URL does speak this
// protocol. Any other 404/405 is a configuration error (an unknown product, a
// base URL that does not speak the protocol): it is loud, never absorbed by a
// backup engine. A server MUST therefore use 501 or 404/405 for nothing but "no
// such route" on ai/score, and MUST put an ErrorResponse body on every
// application error, including a 404 for an unknown product. Old clients never
// call ai/score. Unknown JSON fields are ignored on both sides.
//
// Errors on the decision routes (ai/decision, ai/score) map to engine-neutral
// outcomes: 401/403 or code "auth" is an authentication failure; 400/422 or code
// "invalid" is a request the engine refused; 429 with code "quota" is an
// exhausted allowance (not transient, never retried, never failed over to a paid
// backup unless the caller opted in); any other 429 ("rate_limited") and every
// 5xx is a transient engine fault. A Retry-After header (seconds or an HTTP date;
// or ErrorResponse.error.retryAfterMs) on any error is honoured as a minimum wait:
// the client does not retry such a response itself, and a circuit breaker keeps
// the engine out of service at least that long (capped at 10 minutes).
//
// A decision from ai/decision is a claim, not a verdict. The client trusts a
// server's calibrated flag and scores, but never lets a response make a decision
// actionable or deterministic: any actionable "outcome" in the body is dropped
// (only a refusal such as "uncertain" is kept), and protocol version 1 has no wire
// form for "deterministic" (that class is declared in-process by a rule table,
// decision.Deterministic, through a field no JSON can set), so a server answer is
// at most calibrated and otherwise self-reported. Unknown fields are ignored.
//
// GET ai/usage answers with a UsageResponse: a client takes a 2xx JSON object as
// "this base URL speaks the protocol" only when it has that shape (a non-empty
// "product" string, and "allowance" an object or null when present), so a catch-all
// 200 from a web host in front of the API does not count.
//
// Every request carries HeaderProtocol (the protocol version this client speaks,
// ProtocolVersion), in addition to the version prefix of {base}; a server may use
// it to answer a newer or older client in the version it asked for, and ScoreResponse
// echoes the version it answered in.
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
	PathScore       = "ai/score"
	PathUsage       = "ai/usage"
	PathInteraction = "ai/interactions"

	HeaderProduct = "X-AI-Product"
	// HeaderProtocol carries the protocol version the client speaks (the value is
	// ProtocolVersion, as a decimal string).
	HeaderProtocol = "X-AI-Protocol"
	ContentTypeSSE = "text/event-stream"

	// ProtocolVersion is this client's protocol version. It names the shape of the
	// routes above; a change that old clients cannot ignore bumps it (and the
	// version prefix of {base}).
	ProtocolVersion = 1
)

// DecisionResponse is the body of POST ai/decision.
type DecisionResponse struct {
	Decided  bool              `json:"decided"`
	Decision decision.Decision `json:"decision,omitzero"`
	Model    string            `json:"model,omitempty"`
	Usage    *ai.Usage         `json:"usage,omitempty"`
}

// ScoreRequest is the body of POST ai/score: a decision.ScoreRequest (product,
// text, context, questions) with the same correlation fields a decision request
// carries. The text, context and candidate descriptions reach the server's
// decision engines verbatim: callers send metadata, never row data.
type ScoreRequest struct {
	decision.ScoreRequest
	InteractionID string            `json:"interactionId,omitempty"`
	ClientContext *ai.ClientContext `json:"clientContext,omitempty"`
}

// ScoreResponse is the body of a 2xx response to POST ai/score: one answer per
// question (answers keyed by question id, scores per candidate), plus how the
// server produced them.
type ScoreResponse struct {
	// Answers has one entry per requested question id. Each decision.Answer carries
	// its scores (best first or not: the client sorts), its own Confidence when
	// HasConfidence, and its own Calibrated flag.
	Answers map[string]decision.Answer `json:"answers"`
	// Engine is the leaf engine that answered (for example "jev"), Model the
	// engine's model id, Strategy the server's combinator ("single", "fallback",
	// "hedged", "race" or empty).
	Engine   string `json:"engine,omitempty"`
	Model    string `json:"model,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	// Calibrated is true only when the answering engine produced calibrated
	// probabilities. A client treats an answer as calibrated only when this and the
	// answer's own flag are both true; false (an LLM emulator behind a fallback)
	// makes every answer a proposal under a selection policy.
	Calibrated bool `json:"calibrated"`
	// Attempts lists each engine tried, in start order, with its outcome (the
	// decision.Attempt outcomes), so a client's trace shows a fallback.
	// Each attempt carries its own usage when its engine reported it (so a hedged
	// or fallen-back call can be metered per engine), and its latency in the
	// integer "latencyMs" (milliseconds; see decision.Attempt).
	Attempts []decision.Attempt `json:"attempts,omitempty"`
	// Usage is what the answering engine consumed. Per-attempt usage is on the
	// attempts.
	Usage *ai.Usage `json:"usage,omitempty"`
	// Protocol is the protocol version the server answered in (informational; 0
	// when the server does not say).
	Protocol int `json:"protocol,omitempty"`
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
