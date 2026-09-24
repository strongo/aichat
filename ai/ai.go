// Package ai is the product-neutral AI contract shared by Sneat, DataTug and
// future products: chat requests, the normalised streaming event model, and
// the LLMProvider interface every adapter (the ai/cloud client, OpenAI-compatible,
// Anthropic) implements.
//
// Products own their scopes, actions, prompts and controls. This package owns
// only what every product needs to talk to a model the same way.
package ai

import (
	"context"
	"encoding/json"
	"iter"
	"time"
)

// Role of a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversation turn. The system prompt and context travel
// separately (ChatRequest.System, ChatRequest.Context) so adapters can place
// them where each provider caches best.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
}

// ContextKind separates stable context (instructions, schemas, skills,
// capabilities — good prompt-cache candidates) from per-turn data (current
// time, entities) that changes often.
type ContextKind string

const (
	ContextStatic  ContextKind = "static"
	ContextDynamic ContextKind = "dynamic"
)

// ContextBlock is one named piece of context for a scope, e.g. the calendar
// skill (static) or the user's happenings this week (dynamic). Scope names are
// product-defined strings; this package attaches no meaning to them.
type ContextBlock struct {
	Scope string      `json:"scope"`
	Kind  ContextKind `json:"kind"`
	Name  string      `json:"name"`
	Text  string      `json:"text"`
}

// ModelAuto asks the serving side to pick the model (ai/cloud routing).
// BYOK adapters treat it as "use the configured model".
const ModelAuto = "auto"

// ChatRequest is a single streamed inference request.
type ChatRequest struct {
	// Product identifies the consuming product ("sneat", "datatug", ...) for
	// cloud metering, limits and routing. BYOK adapters ignore it.
	Product string `json:"product"`
	// Model is a concrete model ID, ModelAuto, or "" (same as ModelAuto).
	Model string `json:"model,omitempty"`
	// System is the stable system prompt.
	System string `json:"system,omitempty"`
	// Context blocks are rendered after System, static blocks first, in the
	// given order, so the static prefix is cacheable.
	Context  []ContextBlock `json:"context,omitempty"`
	Messages []Message      `json:"messages"`
	// MaxTokens caps output; 0 means the adapter default.
	MaxTokens int `json:"maxTokens,omitempty"`
	// ResponseSchema, when set, asks for a JSON object matching this JSON
	// Schema. Adapters use native structured output where the provider has it
	// and otherwise instruct the model; either way the final object arrives as
	// an EventStructured event (text deltas may still stream first).
	ResponseSchema json.RawMessage `json:"responseSchema,omitempty"`
	// StrictSchema controls whether an adapter with a native "strict" JSON
	// Schema mode (e.g. OpenAI's response_format.json_schema.strict) turns
	// it on for ResponseSchema. Strict mode requires the schema to follow
	// stricter authoring rules (every property required, no bare optional
	// fields, additionalProperties:false throughout); a caller whose schema
	// doesn't meet them sets StrictSchema to a false pointer to opt out. Nil
	// (the default) means strict when the adapter supports it.
	StrictSchema *bool `json:"strictSchema,omitempty"`
	// Metadata is opaque key/value data forwarded to the cloud for diagnostics
	// (e.g. "path": "llm-fallback"). Never put secrets or user content here.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// EventType names a normalised stream event. The string values are also the
// SSE event names of the cloud protocol (see package cloudproto).
type EventType string

const (
	EventStarted    EventType = "response.started"
	EventTextDelta  EventType = "text.delta"
	EventStructured EventType = "output.structured"
	EventUsage      EventType = "usage"
	EventError      EventType = "error"
	EventCompleted  EventType = "response.completed"
)

// Event is one normalised stream event. Exactly the fields relevant to Type
// are set. Future tool/action events add new EventType values and fields;
// consumers must ignore event types they do not know.
type Event struct {
	Type EventType `json:"type"`
	// Started: which provider/model is actually answering.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// TextDelta: the next chunk of text.
	Text string `json:"text,omitempty"`
	// Structured: the final JSON object for ChatRequest.ResponseSchema.
	Structured json.RawMessage `json:"structured,omitempty"`
	// Usage / Completed: token and allowance accounting when known.
	Usage *Usage `json:"usage,omitempty"`
	// Error: a terminal or non-terminal provider error.
	Error *Error `json:"error,omitempty"`
}

// Usage is token and allowance accounting for one response.
type Usage struct {
	InputTokens      int64 `json:"inputTokens,omitempty"`
	OutputTokens     int64 `json:"outputTokens,omitempty"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// Allowance is set by the cloud provider (ai/cloud); nil for BYOK.
	Allowance *Allowance `json:"allowance,omitempty"`
}

// Allowance is the caller's cloud quota after this response.
type Allowance struct {
	Unit     string    `json:"unit"` // e.g. "tokens", "requests"
	Used     int64     `json:"used"`
	Limit    int64     `json:"limit"`
	ResetsAt time.Time `json:"resetsAt,omitzero"`
}

// Error codes shared by all adapters and the cloud protocol.
const (
	ErrCodeAuth        = "auth"         // missing/invalid credentials
	ErrCodeQuota       = "quota"        // allowance exhausted
	ErrCodeRateLimited = "rate_limited" // retry later
	ErrCodeUpstream    = "upstream"     // provider failure
	ErrCodeInvalid     = "invalid"      // bad request
	ErrCodeCanceled    = "canceled"
)

// Error is a provider error normalised for display and fallback decisions.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsRetryable reports whether the caller may retry the request that produced
// this error. It lets *Error satisfy ai/internal/retry.Retryable.
func (e *Error) IsRetryable() bool { return e != nil && e.Retryable }

// LLMProvider streams one chat response.
//
// Stream yields events in order: EventStarted (only once the HTTP request
// has actually succeeded; a request that fails before any response is
// received produces no EventStarted at all), then any number of
// EventTextDelta / EventStructured / EventUsage, then exactly one of:
//
//   - EventCompleted, ending the sequence successfully, or
//   - a FATAL error: exactly one final yield of
//     (Event{Type: EventError, Error: e}, e), where e is a non-nil *Error
//     (wrapped in the returned error). Nothing is yielded after it, and the
//     implementation must return immediately afterward.
//
// An EventError yielded with a nil Go error (second return value) is NOT
// fatal -- it reports a problem the stream is continuing past (e.g. a
// dropped mid-stream diagnostic) and consumers must keep ranging.
// Implementations must not buffer the full response and must stop promptly
// when ctx is cancelled or the consumer stops iterating. A stream ended by
// context cancellation must yield the fatal pair with Code ErrCodeCanceled
// (never Retryable).
type LLMProvider interface {
	// Name identifies the provider in diagnostics ("cloud",
	// "openai-compatible", "anthropic").
	Name() string
	Stream(ctx context.Context, req ChatRequest) iter.Seq2[Event, error]
}

// Collect drains a stream into its concatenated text, the last structured
// output and the last usage seen. It is a convenience for non-interactive
// callers and tests; interactive UIs should consume Stream directly.
//
// Per the LLMProvider contract, a non-nil error (the iterator's second
// value) is always fatal and always terminates the stream, so Collect
// returns as soon as it sees one. A non-fatal EventError (nil Go error) is
// NOT terminal: Collect keeps draining past it, since the stream itself
// says it is continuing.
func Collect(stream iter.Seq2[Event, error]) (text string, structured json.RawMessage, usage *Usage, err error) {
	var b []byte
	for ev, e := range stream {
		if e != nil {
			return string(b), structured, usage, e
		}
		switch ev.Type {
		case EventTextDelta:
			b = append(b, ev.Text...)
		case EventStructured:
			structured = ev.Structured
		case EventUsage, EventCompleted:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		}
	}
	return string(b), structured, usage, nil
}
