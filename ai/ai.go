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
	// RoleTool carries ToolResults answering a prior assistant ToolCalls
	// message. Adapters translate it to whatever the provider needs (e.g.
	// Anthropic user messages with tool_result blocks, OpenAI-compatible
	// role:"tool" messages).
	RoleTool Role = "tool"
)

// Message is one conversation turn. The system prompt and context travel
// separately (ChatRequest.System, ChatRequest.Context) so adapters can place
// them where each provider caches best.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
	// ToolCalls is set on an assistant message that invoked tools.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolResults is set on a RoleTool message answering prior ToolCalls.
	ToolResults []ToolResult `json:"toolResults,omitempty"`
	// ProviderState is opaque, provider-specific extra content an adapter
	// attached to an assistant message it produced (e.g. ai/anthropic's
	// extended-thinking/redacted-thinking blocks, signature included) and
	// that same adapter MUST replay unmodified on a later request that
	// includes this message — some providers 400 a tool-use continuation
	// that drops or edits the thinking blocks from the turn that requested
	// the tool call. Populated from Event.ProviderState (see EventCompleted)
	// by whoever appends the assistant message (e.g. ai/agent.Loop). An
	// adapter that doesn't understand another adapter's ProviderState MUST
	// ignore it rather than error.
	ProviderState json.RawMessage `json:"providerState,omitempty"`
}

// Tool is a function the model may call. Schema is the JSON Schema of the
// arguments object (not the whole tool envelope).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// ToolCall is one invocation the model asked for, with its arguments already
// assembled from any streamed deltas.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult answers a ToolCall by CallID. Content is provider-facing text
// (JSON-encode structured results yourself); IsError marks a tool-level
// failure (as opposed to an infrastructure failure, which aborts the run).
type ToolResult struct {
	CallID  string `json:"callId"`
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
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
	// Tools the model may call this turn.
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice: "" (adapter default) | "auto" | "none" | "required" | a
	// specific tool name.
	ToolChoice string `json:"toolChoice,omitempty"`
	// Reasoning requests extended/deliberate reasoning where the provider
	// supports it: "" | "low" | "medium" | "high". Adapters map it to their
	// own knob (OpenAI-compatible reasoning_effort, Anthropic extended
	// thinking budget) and ignore it where unsupported.
	Reasoning string `json:"reasoning,omitempty"`
}

// ToolChoice values for ChatRequest.ToolChoice.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
)

// Reasoning effort levels for ChatRequest.Reasoning.
const (
	ReasoningLow    = "low"
	ReasoningMedium = "medium"
	ReasoningHigh   = "high"
)

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
	// EventToolCall is emitted once per call, fully assembled (adapters buffer
	// streamed argument deltas), before the terminal EventCompleted of that
	// response.
	EventToolCall EventType = "tool.call"
	// EventToolResult is emitted only by ai/agent as it feeds tool results
	// back into the loop; adapters never emit it.
	EventToolResult EventType = "tool.result"
)

// StopReason values for Event.StopReason on EventCompleted.
const (
	StopReasonToolCalls = "tool_calls"
	StopReasonEnd       = "end"
	StopReasonLength    = "length"
	// StopReasonRefusal: the provider's own safety layer declined to
	// answer (e.g. Anthropic stop_reason "refusal"). Not an ai.Error --
	// the response completed normally, just with no usable content.
	StopReasonRefusal = "refusal"
	// StopReasonPauseTurn: the provider paused mid-turn expecting the
	// caller to continue the SAME turn with another request (e.g.
	// Anthropic stop_reason "pause_turn", used with long-running
	// server-side tools). Not a stopping point a caller should treat as
	// "done" the way StopReasonEnd is.
	StopReasonPauseTurn = "pause_turn"
	// StopReasonContentFilter: the provider stopped generation because a
	// content filter flagged the response (e.g. ai/openairesponses'
	// response.incomplete with incomplete_details.reason
	// "content_filter"). Like StopReasonRefusal, not an ai.Error -- the
	// response completed, just with content the provider declined to
	// finish delivering.
	StopReasonContentFilter = "content_filter"
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
	// ToolCall: set on EventToolCall, one fully-assembled call.
	ToolCall *ToolCall `json:"toolCall,omitempty"`
	// ToolResult: set on EventToolResult (ai/agent only).
	ToolResult *ToolResult `json:"toolResult,omitempty"`
	// StopReason: set on EventCompleted; "tool_calls" | "end" | "length".
	StopReason string `json:"stopReason,omitempty"`
	// ProviderState: set on EventCompleted when the adapter captured
	// provider-specific state (e.g. ai/anthropic's thinking/
	// redacted_thinking blocks with signatures) that MUST be attached to the
	// assistant message this turn produces — see Message.ProviderState.
	ProviderState json.RawMessage `json:"providerState,omitempty"`
}

// Usage is token and allowance accounting for one response.
//
// The fields below are populated from each adapter's native usage object,
// and their SUBSET-VS-ADDITIVE relationship to InputTokens/OutputTokens is
// NOT the same across adapters — summing them naively double-counts on one
// adapter and undercounts on the other. See CacheReadTokens/
// CacheWriteTokens/ReasoningTokens doc below for the per-adapter semantics,
// and BillableTokens for a helper that sums correctly for a named adapter.
type Usage struct {
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	// CacheReadTokens counts tokens served from a prompt cache.
	// ai/openaicompat populates it from
	// usage.prompt_tokens_details.cached_tokens: this is an INFORMATIONAL
	// SUBSET already counted inside InputTokens (OpenAI's prompt_tokens
	// includes cached tokens; the details object only breaks out how many
	// of them were cache hits) — do NOT add it to InputTokens. ai/anthropic
	// populates it from usage.cache_read_input_tokens: on the Messages API
	// this is the OPPOSITE relationship — Anthropic's input_tokens counts
	// ONLY the tokens actually processed fresh, and cache_read_input_tokens
	// (billed at its own, cheaper per-token rate) is NOT included in it —
	// so for ai/anthropic, CacheReadTokens IS additive to InputTokens.
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`
	// CacheWriteTokens counts tokens written to a prompt cache. Only
	// ai/anthropic populates it, from usage.cache_creation_input_tokens —
	// same additive relationship to InputTokens as CacheReadTokens above
	// (billed separately, at its own higher per-token rate, and not
	// included in input_tokens). ai/openaicompat never populates this
	// field: OpenAI's API has no separate cache-write concept to report.
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	// ReasoningTokens counts provider-side reasoning/thinking tokens, ONLY
	// on adapters that report them SEPARATELY from OutputTokens.
	// ai/openaicompat populates it from
	// usage.completion_tokens_details.reasoning_tokens when the API returns
	// that field: this is an INFORMATIONAL SUBSET already counted inside
	// OutputTokens (OpenAI's completion_tokens includes reasoning tokens;
	// the details object only breaks out how many of them were spent on
	// reasoning) — do NOT add it to OutputTokens. ai/anthropic leaves it
	// zero: the Messages API's usage object has no separate thinking-token
	// count — thinking tokens are already included in OutputTokens
	// (usage.output_tokens), not broken out on top of it, so there is
	// nothing distinct to report here without double-counting.
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`
	// Allowance is set by the cloud provider (ai/cloud); nil for BYOK.
	Allowance *Allowance `json:"allowance,omitempty"`
}

// BillableTokens sums u into the total tokens the named provider actually
// bills for this response, without double-counting a subset field (see the
// per-field doc on Usage) against the total it is already included in.
// provider should be the ai.LLMProvider.Name() that produced this Usage:
//
//   - "openai-compatible" and "openai-responses": CacheReadTokens/
//     ReasoningTokens are informational subsets already counted inside
//     InputTokens/OutputTokens — Total = InputTokens + OutputTokens.
//     ai/openairesponses populates them from the Responses API's
//     usage.input_tokens_details.cached_tokens and
//     usage.output_tokens_details.reasoning_tokens, the same subset
//     relationship as ai/openaicompat's Chat Completions
//     prompt_tokens_details/completion_tokens_details fields.
//   - "anthropic" (and any other/unrecognised provider name — see below):
//     CacheReadTokens/CacheWriteTokens are billed separately from
//     InputTokens/OutputTokens — Total = InputTokens + OutputTokens +
//     CacheReadTokens + CacheWriteTokens. ReasoningTokens is not added:
//     no current adapter populates it additively.
//
// An unrecognised provider name falls back to the additive (Anthropic-
// style) formula: silently ignoring a populated Cache*Tokens field would
// undercount real spend, which is the worse failure mode for a billing
// total than adding a field a future adapter turns out to already include
// (there is currently no adapter where that would happen). Prefer passing
// the adapter's own Name() over relying on this default for a provider
// this function doesn't know the convention of.
func (u Usage) BillableTokens(provider string) int64 {
	switch provider {
	case "openai-compatible", "openai-responses":
		return u.InputTokens + u.OutputTokens
	default:
		return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
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
	// "openai-compatible", "openai-responses", "anthropic").
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
