// Package openairesponses is an ai.LLMProvider over OpenAI's Responses API
// (POST {base}/responses, stream:true), implemented directly over net/http --
// no vendor SDK. It mirrors ai/openaicompat's Config shape and adapter
// conventions but speaks the Responses API's item-based input/output model
// and SSE event set (response.created, response.output_text.delta,
// response.function_call_arguments.delta/done, response.output_item.added/
// done, response.refusal.delta, response.completed, response.failed,
// response.incomplete, error) instead of Chat Completions' delta/choices
// shape.
package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/internal/retry"
	"github.com/strongo/aichat/ai/internal/sse"
)

// Config configures a Provider. BaseURL and APIKey are required; Model is the
// default used when a ChatRequest leaves Model empty or ai.ModelAuto. Same
// shape as ai/openaicompat.Config and ai/anthropic.Config so products can
// swap adapters without reshaping their wiring.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Headers    map[string]string
	HTTPClient *http.Client
}

// Provider implements ai.LLMProvider over the Responses API.
type Provider struct {
	cfg Config

	// noReasoningMu guards noReasoning.
	noReasoningMu sync.Mutex
	// noReasoning records, per MODEL (a Provider can be reused across
	// requests naming different models, and support for the field is a
	// property of the model/deployment), that this Provider has learned,
	// from a live 400 response, that the endpoint rejects the top-level
	// `reasoning` field for that model. Once a model is recorded here,
	// every later Stream call for that model omits `reasoning` from the
	// start, rather than paying the extra round trip on every request.
	// Mirrors ai/openaicompat.Provider.noReasoningEffort.
	noReasoning map[string]bool

	// noEncryptedContentMu guards noEncryptedContent.
	noEncryptedContentMu sync.Mutex
	// noEncryptedContent records, per MODEL, that this Provider has
	// learned, from a live 400 response, that the endpoint rejects
	// include:["reasoning.encrypted_content"] for that model (NB2, r2
	// review: some models 400 with "Encrypted content is not supported
	// with this model"). Once recorded, every later Stream call for that
	// model omits `include` and drops any replayed reasoning item from
	// `input` (it would be equally unreplayable there).
	noEncryptedContent map[string]bool
}

// marshalJSON is json.Marshal, indirected so a test can force the
// marshal-error path deterministically -- Config/ai.ChatRequest never carry
// a Go value json.Marshal itself rejects (chan/func/complex), so that path
// is otherwise unreachable through the public API.
var marshalJSON = json.Marshal

// New builds a Provider. It panics if BaseURL is empty.
func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		panic("openairesponses: Config.BaseURL is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Provider{cfg: cfg}
}

// Name implements ai.LLMProvider.
func (p *Provider) Name() string { return "openai-responses" }

// --- request wire types --------------------------------------------------

// inputItem is one element of the Responses API "input" array. It is built
// via messageItem/functionCallItem/functionCallOutputItem/rawInputItem
// (never constructed directly): each kind of item has its OWN required
// fields, so dispatching on kind at marshal time -- rather than one struct
// with `omitempty` on every field -- is what lets an EMPTY message/output
// string still reach the wire. The Responses API requires `content` on a
// message item and `output` on a function_call_output item even when they
// are the empty string; `omitempty` would silently drop the field entirely
// instead of sending `""`.
type inputItem struct {
	// raw, when non-nil, is a captured output item (see providerStateBlocks
	// / Event.ProviderState) replayed onto the wire VERBATIM -- see
	// buildInput's ProviderState-replay branch. It takes priority over
	// every other field.
	raw json.RawMessage

	kind      string // "message" | "function_call" | "function_call_output"
	role      string
	content   string
	callID    string
	name      string
	arguments string
	output    string
}

func messageItem(role, content string) inputItem {
	return inputItem{kind: "message", role: role, content: content}
}

func functionCallItem(callID, name, arguments string) inputItem {
	return inputItem{kind: "function_call", callID: callID, name: name, arguments: arguments}
}

func functionCallOutputItem(callID, output string) inputItem {
	return inputItem{kind: "function_call_output", callID: callID, output: output}
}

// rawInputItem wraps a captured output item to be replayed verbatim (see
// buildInput).
func rawInputItem(raw json.RawMessage) inputItem { return inputItem{raw: raw} }

// MarshalJSON dispatches on kind so each item type gets exactly its own
// required fields (see the inputItem doc for why this isn't one struct with
// omitempty tags), or is emitted byte-for-byte when raw is set.
func (it inputItem) MarshalJSON() ([]byte, error) {
	if len(it.raw) > 0 {
		return it.raw, nil
	}
	switch it.kind {
	case "function_call":
		return json.Marshal(struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{"function_call", it.callID, it.name, it.arguments})
	case "function_call_output":
		return json.Marshal(struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}{"function_call_output", it.callID, it.output})
	default: // "message"
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{it.role, it.content})
	}
}

type toolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict"`
}

type namedToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type reasoningOpt struct {
	Effort string `json:"effort"`
}

type jsonSchemaFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type textOpt struct {
	Format jsonSchemaFormat `json:"format"`
}

// responseIncludeReasoningEncryptedContent asks the Responses API to return
// a reasoning item's encrypted_content inline in the stream (rather than
// requiring server-side response storage to retrieve it later). Combined
// with Store:false below, this is what makes cross-turn reasoning replay
// (REQ: openairesponses-adapter, the B1 fix) possible without ever
// persisting a conversation on OpenAI's servers.
const responseIncludeReasoningEncryptedContent = "reasoning.encrypted_content"

type responseRequestBody struct {
	Model           string      `json:"model"`
	Input           []inputItem `json:"input"`
	Instructions    string      `json:"instructions,omitempty"`
	Stream          bool        `json:"stream"`
	MaxOutputTokens int         `json:"max_output_tokens,omitempty"`
	Tools           []toolDef   `json:"tools,omitempty"`
	// ToolChoice is either a bare string ("auto"|"none"|"required") or a
	// namedToolChoice{"type":"function","name":...} (Responses API keeps
	// tool_choice flat, unlike Chat Completions' nested function object).
	ToolChoice any           `json:"tool_choice,omitempty"`
	Reasoning  *reasoningOpt `json:"reasoning,omitempty"`
	Text       *textOpt      `json:"text,omitempty"`
	// Store is ALWAYS false: this adapter never relies on OpenAI's
	// server-side conversation state (no `previous_response_id` chaining).
	// It has no `omitempty` -- `false` is the zero value, and the field
	// must still be sent explicitly so the API never falls back to its own
	// default (which may differ by account/plan).
	Store bool `json:"store"`
	// Include always requests reasoning.encrypted_content so a reasoning
	// item's ProviderState-captured JSON is self-contained and replayable
	// with Store:false (see responseIncludeReasoningEncryptedContent).
	Include []string `json:"include,omitempty"`
}

// --- response wire types --------------------------------------------------

type outputItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // "message" | "function_call" | "reasoning" | ...
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type responseAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

type tokenDetails struct {
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type responseUsage struct {
	InputTokens         int64         `json:"input_tokens"`
	OutputTokens        int64         `json:"output_tokens"`
	InputTokensDetails  *tokenDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *tokenDetails `json:"output_tokens_details,omitempty"`
}

type responseObj struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	Output            []outputItem       `json:"output,omitempty"`
	Usage             *responseUsage     `json:"usage,omitempty"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details,omitempty"`
	Error             *responseAPIError  `json:"error,omitempty"`
}

// sseEvent is the union of every field any Responses API streaming event
// payload may carry. Only the fields relevant to Type are populated by the
// server; this adapter reads Type first and only looks at the fields that
// event defines (see Stream). Item is kept as raw JSON (rather than
// unmarshalled straight into outputItem) so response.output_item.done can
// both (a) inspect the typed fields it needs (call_id/name/type) AND (b)
// capture the item's exact bytes for ProviderState replay (REQ:
// openairesponses-adapter, B1) -- decoding straight into outputItem would
// silently drop every field this adapter doesn't itself model (e.g. a
// reasoning item's `encrypted_content`/`summary`), corrupting the replay.
type sseEvent struct {
	Type      string          `json:"type"`
	Response  *responseObj    `json:"response,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	Item      json.RawMessage `json:"item,omitempty"`
	Delta     string          `json:"delta,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Code      string          `json:"code,omitempty"`
	Message   string          `json:"message,omitempty"`
}

type apiErrorBody struct {
	Error responseAPIError `json:"error"`
}

// Responses API SSE event type names this adapter understands. Any other
// event type (response.in_progress, response.output_text.done,
// response.content_part.*, reasoning-summary/MCP/web-search progress
// events, ...) is silently ignored, per the "consumers must ignore event
// types they do not know" clause of the ai.LLMProvider contract.
const (
	evResponseCreated    = "response.created"
	evOutputTextDelta    = "response.output_text.delta"
	evOutputItemAdded    = "response.output_item.added"
	evOutputItemDone     = "response.output_item.done"
	evFuncArgsDelta      = "response.function_call_arguments.delta"
	evFuncArgsDone       = "response.function_call_arguments.done"
	evRefusalDelta       = "response.refusal.delta"
	evResponseCompleted  = "response.completed"
	evResponseFailed     = "response.failed"
	evResponseIncomplete = "response.incomplete"
	evError              = "error"
)

// Stream implements ai.LLMProvider.
//
// Fatal-error contract (see ai.LLMProvider doc): every fatal condition
// yields exactly one final (ai.Event{Type: ai.EventError, Error: e}, e) and
// returns; EventStarted is only yielded once the HTTP request has actually
// succeeded. The Responses API has no wire "[DONE]" sentinel like Chat
// Completions -- its own terminal events are response.completed (success)
// and response.incomplete (truncated, e.g. by max_output_tokens); either
// maps to exactly one ai.EventCompleted, so callers never see the wire-level
// distinction the "truncation = no response.completed" doc comment on
// ai.LLMProvider warns about. response.incomplete while ANY tool call is
// still open/assembled (never fully confirmed by output_item.done) is
// instead FATAL -- a partial/possibly-invalid-JSON tool call is not safe to
// hand to any consumer, regardless of whether ResponseSchema was requested.
func (p *Provider) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		model := req.Model
		if model == "" || model == ai.ModelAuto {
			model = p.cfg.Model
		}

		body := responseRequestBody{
			Model:           model,
			Input:           buildInput(req),
			Instructions:    buildInstructions(req),
			Stream:          true,
			MaxOutputTokens: req.MaxTokens,
			Store:           false,
		}
		wantStructured := len(req.ResponseSchema) > 0
		if wantStructured {
			strict := req.StrictSchema == nil || *req.StrictSchema
			body.Text = &textOpt{Format: jsonSchemaFormat{
				Type:   "json_schema",
				Name:   "response",
				Schema: req.ResponseSchema,
				Strict: strict,
			}}
		}
		if len(req.Tools) > 0 {
			body.Tools = make([]toolDef, len(req.Tools))
			for i, t := range req.Tools {
				body.Tools[i] = toolDef{
					Type:        "function",
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Schema,
					Strict:      false,
				}
			}
			body.ToolChoice = toolChoiceWire(req.ToolChoice)
		}
		sentReasoning := req.Reasoning != "" && !p.reasoningUnsupported(model)
		if sentReasoning {
			body.Reasoning = &reasoningOpt{Effort: req.Reasoning}
		}
		// NB2 (r2 review): include:["reasoning.encrypted_content"] itself
		// 400s on a model that doesn't support encrypted reasoning content
		// at all ("Encrypted content is not supported with this model").
		// Once THAT is learned for a model, this adapter stops asking for
		// it, and -- since a captured reasoning item is then unreplayable
		// on that model regardless of what an earlier turn captured --
		// also drops any replayed reasoning item from Input up front,
		// rather than only reacting after a second failed request.
		sentInclude := !p.encryptedContentUnsupported(model)
		if sentInclude {
			body.Include = []string{responseIncludeReasoningEncryptedContent}
		} else {
			body.Input = dropReasoningItems(body.Input)
		}
		payload, err := marshalJSON(body)
		if err != nil {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeInvalid, Message: err.Error()})
			return
		}

		send := func(payload []byte) (*http.Response, error) {
			var resp *http.Response
			attempt := 0
			doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
				attempt++
				r, e := p.doRequest(ctx, payload, attempt >= retry.DefaultMaxAttempts)
				resp = r
				return e
			})
			return resp, doErr
		}

		resp, doErr := send(payload)
		if doErr != nil {
			switch {
			case sentInclude && isUnsupportedEncryptedContentError(doErr):
				// NB2 (r2 review): retry ONCE, before any byte of a
				// response was seen, without `include` -- and without any
				// reasoning item that was about to be replayed, since this
				// model can't accept one back either -- remembering not to
				// send `include` again for this MODEL.
				p.markEncryptedContentUnsupported(model)
				body.Include = nil
				body.Input = dropReasoningItems(body.Input)
				if retryPayload, merr := marshalJSON(body); merr == nil {
					resp, doErr = send(retryPayload)
				}
			case sentReasoning && isUnsupportedReasoningError(doErr):
				// M2 (r1 review): some deployments 400 on an unrecognised
				// top-level `reasoning` field instead of ignoring it. Retry
				// ONCE, before any byte of a response was seen, without it
				// -- and remember not to send it again on this Provider
				// instance, per MODEL (mirrors ai/openaicompat's identical
				// fallback for Chat Completions' reasoning_effort).
				p.markReasoningUnsupported(model)
				body.Reasoning = nil
				if retryPayload, merr := marshalJSON(body); merr == nil {
					resp, doErr = send(retryPayload)
				}
			}
		}
		if doErr != nil {
			yieldFatal(yield, toAIError(ctx, doErr))
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !yield(ai.Event{Type: ai.EventStarted, Provider: p.Name(), Model: model}, nil) {
			return
		}

		var structuredBuf strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		sc.Split(sse.ScanLines)
		asm := newCallAssembler()
		var usage *ai.Usage
		var providerItems []json.RawMessage
		sawTerminal := false
		sawRefusal := false
		stopReason := ai.StopReasonEnd
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			var ev sseEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: fmt.Sprintf("openairesponses: bad event: %v", err)})
				return
			}

			switch ev.Type {
			case evOutputTextDelta:
				if ev.Delta == "" {
					continue
				}
				if wantStructured {
					structuredBuf.WriteString(ev.Delta)
				}
				if !yield(ai.Event{Type: ai.EventTextDelta, Text: ev.Delta}, nil) {
					return
				}

			case evRefusalDelta:
				// A refusal is not an ai.Error -- the response completes
				// normally, just with no usable content (see
				// ai.StopReasonRefusal's doc) -- so the refusal text is
				// NOT surfaced as EventTextDelta; only StopReason changes,
				// on completion below.
				sawRefusal = true

			case evOutputItemAdded:
				item := decodeOutputItem(ev.Item)
				if item != nil && item.Type == "function_call" {
					asm.add(item.ID, item.CallID, item.Name)
				}

			case evFuncArgsDelta:
				if ev.ItemID != "" && ev.Delta != "" {
					asm.appendArgs(ev.ItemID, ev.Delta)
				}

			case evFuncArgsDone:
				if ev.ItemID != "" && ev.Arguments != "" {
					asm.setArgs(ev.ItemID, ev.Arguments)
				}

			case evOutputItemDone:
				// Every output item (message, reasoning, function_call,
				// ...) is captured verbatim, in stream order, for
				// ProviderState replay (REQ: openairesponses-adapter, B1)
				// -- not just function_call items.
				if len(ev.Item) > 0 {
					providerItems = append(providerItems, append(json.RawMessage(nil), ev.Item...))
				}
				item := decodeOutputItem(ev.Item)
				if item != nil && item.Type == "function_call" {
					asm.finalize(item.ID, item.CallID, item.Name, item.Arguments)
				}

			case evResponseCompleted:
				sawTerminal = true
				if ev.Response != nil {
					usage = usageFromWire(ev.Response.Usage)
				}
				switch {
				case asm.len() > 0:
					stopReason = ai.StopReasonToolCalls
				case sawRefusal:
					stopReason = ai.StopReasonRefusal
				}

			case evResponseIncomplete:
				sawTerminal = true
				if ev.Response != nil {
					usage = usageFromWire(ev.Response.Usage)
				}
				if asm.len() > 0 {
					// B2 (r1 review): a truncated response with any
					// open/assembled tool call is ALWAYS fatal -- its
					// arguments JSON may be incomplete/invalid, and there
					// is no safe partial ai.ToolCall to hand a consumer.
					yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "openairesponses: response truncated with an open tool call in flight"})
					return
				}
				reason := ""
				if ev.Response != nil && ev.Response.IncompleteDetails != nil {
					reason = ev.Response.IncompleteDetails.Reason
				}
				switch reason {
				case "max_output_tokens":
					stopReason = ai.StopReasonLength
				case "content_filter":
					stopReason = ai.StopReasonContentFilter
				default:
					stopReason = ai.StopReasonEnd
				}
				if wantStructured && stopReason == ai.StopReasonLength {
					yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "openairesponses: response truncated at max_output_tokens before a complete structured JSON object was produced"})
					return
				}

			case evResponseFailed:
				code, msg := "", ""
				if ev.Response != nil && ev.Response.Error != nil {
					code, msg = ev.Response.Error.Code, ev.Response.Error.Message
				}
				if msg == "" {
					msg = "openairesponses: response failed"
				}
				yieldFatal(yield, classifyStreamError(code, msg))
				return

			case evError:
				msg := ev.Message
				if msg == "" {
					msg = "openairesponses: stream error"
				}
				yieldFatal(yield, classifyStreamError(ev.Code, msg))
				return

			default:
				// evResponseCreated and every other event type this adapter
				// doesn't need are ignored.
			}
			if sawTerminal {
				break
			}
		}
		if err := sc.Err(); err != nil {
			yieldFatal(yield, toAIError(ctx, err))
			return
		}
		if !sawTerminal {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "openairesponses: stream truncated (no response.completed/response.incomplete)"})
			return
		}

		if usage != nil {
			if !yield(ai.Event{Type: ai.EventUsage, Usage: usage}, nil) {
				return
			}
		}

		if wantStructured && structuredBuf.Len() > 0 {
			raw := extractJSON(structuredBuf.String())
			if !yield(ai.Event{Type: ai.EventStructured, Structured: json.RawMessage(raw)}, nil) {
				return
			}
		}

		for _, call := range asm.calls() {
			c := call
			if !yield(ai.Event{Type: ai.EventToolCall, ToolCall: &c}, nil) {
				return
			}
		}

		var providerState json.RawMessage
		if len(providerItems) > 0 {
			callIDs := asm.synthesizedCallIDs()
			sanitized := make([]json.RawMessage, len(providerItems))
			for i, raw := range providerItems {
				sanitized[i] = sanitizeProviderItem(raw, callIDs)
			}
			if b, err := json.Marshal(sanitized); err == nil {
				providerState = b
			}
		}
		yield(ai.Event{Type: ai.EventCompleted, Usage: usage, StopReason: stopReason, ProviderState: providerState}, nil)
	}
}

// decodeOutputItem best-effort decodes raw (an SSE event's "item" field)
// into an outputItem, returning nil when raw is empty or unparsable rather
// than erroring the whole stream over a field this adapter merely uses for
// tool-call assembly (ProviderState replay uses the untouched raw bytes
// instead -- see evOutputItemDone).
func decodeOutputItem(raw json.RawMessage) *outputItem {
	if len(raw) == 0 {
		return nil
	}
	var item outputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil
	}
	return &item
}

// sanitizeProviderItem prepares a captured output item (raw, exactly as
// response.output_item.done sent it) for storage in Event.ProviderState /
// eventual replay in buildInput:
//
//   - NB1 (r2 review): the item's top-level `id` and `status` are ALWAYS
//     stripped. With Store:false, OpenAI never persists items server-side,
//     and replaying an item's own `id` verbatim 400s ("Item with id
//     'rs_...' not found. Items are not persisted when store is set to
//     false") -- everything else, including a reasoning item's
//     encrypted_content, is kept untouched.
//   - minor (r2 review): for a function_call item, `call_id` is
//     overwritten with callIDs' entry for this item's OWN `id` (looked up
//     from raw BEFORE it's stripped) -- the same final call_id
//     callAssembler.calls() assigned that call (real if the provider sent
//     one, else the synthesized "call_N"). Without this, a call whose
//     provider-sent call_id was empty would replay with an empty
//     call_id, while the function_call_output ai/agent builds from the
//     surfaced (synthesized) ai.ToolCall.ID would carry a DIFFERENT
//     value -- an orphaned function_call/function_call_output pair on
//     the wire.
//
// callIDs is keyed by item id (callAssembler.synthesizedCallIDs()), not by
// call_id. A raw item that fails to parse as a JSON object is returned
// unmodified (best-effort; still valid JSON, just not sanitized).
func sanitizeProviderItem(raw json.RawMessage, callIDs map[string]string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	var head struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &head)

	delete(fields, "id")
	delete(fields, "status")
	if head.Type == "function_call" {
		if callID, ok := callIDs[head.ID]; ok {
			if b, err := json.Marshal(callID); err == nil {
				fields["call_id"] = b
			}
		}
	}

	b, err := marshalJSON(fields)
	if err != nil {
		return raw
	}
	return b
}

// isReasoningRawItem reports whether raw (a captured output item) is a
// reasoning item, by its wire `type`.
func isReasoningRawItem(raw json.RawMessage) bool {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return false
	}
	return head.Type == "reasoning"
}

// dropReasoningItems returns items with every replayed (raw) reasoning item
// removed -- see NB2 in Stream: once a model is known to reject
// include:["reasoning.encrypted_content"], a captured reasoning item from
// an earlier turn can't be replayed to it either, so it must not be sent at
// all rather than triggering the same rejection a second time. Every other
// item (message/function_call/function_call_output, replayed or freshly
// built) is kept, in order.
func dropReasoningItems(items []inputItem) []inputItem {
	out := make([]inputItem, 0, len(items))
	for _, it := range items {
		if it.raw != nil && isReasoningRawItem(it.raw) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// callAssembler accumulates response.output_item.added/
// response.function_call_arguments.delta+done/response.output_item.done
// events into complete ai.ToolCall values, in the order the items were
// added. output_item.done carries the provider's own fully-assembled
// arguments string, which finalize prefers over the locally-accumulated
// delta buffer -- it is the authoritative source; deltas exist so a UI can
// show live progress, not to replace it.
type callAssembler struct {
	order   []string
	byID    map[string]*ai.ToolCall
	argsBuf map[string]*strings.Builder
}

func newCallAssembler() *callAssembler {
	return &callAssembler{byID: map[string]*ai.ToolCall{}, argsBuf: map[string]*strings.Builder{}}
}

func (a *callAssembler) add(itemID, callID, name string) {
	if _, ok := a.byID[itemID]; ok {
		return
	}
	a.byID[itemID] = &ai.ToolCall{ID: callID, Name: name}
	a.argsBuf[itemID] = &strings.Builder{}
	a.order = append(a.order, itemID)
}

func (a *callAssembler) appendArgs(itemID, delta string) {
	b, ok := a.argsBuf[itemID]
	if !ok {
		// A delta arrived before output_item.added (not expected from the
		// documented event order, but tolerate it rather than dropping
		// data): synthesize the entry.
		a.add(itemID, "", "")
		b = a.argsBuf[itemID]
	}
	b.WriteString(delta)
}

func (a *callAssembler) setArgs(itemID, args string) {
	if _, ok := a.byID[itemID]; !ok {
		a.add(itemID, "", "")
	}
	a.argsBuf[itemID].Reset()
	a.argsBuf[itemID].WriteString(args)
}

// finalize records output_item.done's authoritative call_id/name/arguments
// for itemID, filling in whatever add()/appendArgs() had not yet captured.
func (a *callAssembler) finalize(itemID, callID, name, arguments string) {
	call, ok := a.byID[itemID]
	if !ok {
		a.add(itemID, callID, name)
		call = a.byID[itemID]
	}
	if callID != "" {
		call.ID = callID
	}
	if name != "" {
		call.Name = name
	}
	if arguments != "" {
		a.argsBuf[itemID].Reset()
		a.argsBuf[itemID].WriteString(arguments)
	}
}

func (a *callAssembler) len() int { return len(a.order) }

func (a *callAssembler) calls() []ai.ToolCall {
	out := make([]ai.ToolCall, 0, len(a.order))
	for i, id := range a.order {
		call := *a.byID[id]
		// A malformed/absent call_id must not reach callers as "" --
		// Handler dispatch and ToolResult.CallID pairing both key off it.
		// Synthesize a stable, unique one (mirrors ai/openaicompat's
		// toolCallAssembler.calls()).
		call.ID = synthesizedCallID(call.ID, i)
		args := a.argsBuf[id].String()
		if args == "" {
			args = "{}"
		}
		call.Arguments = json.RawMessage(args)
		out = append(out, call)
	}
	return out
}

// synthesizedCallIDs returns, for every item this assembler tracks (keyed
// by item id, NOT call_id), the SAME final call_id calls() would assign it
// -- real if the provider sent one, else the synthesized "call_N" -- so a
// captured raw output item (see Stream's evOutputItemDone / minor r2
// review) can have its own call_id field patched to match, keeping a
// replayed function_call item and the function_call_output ai/agent builds
// from the matching ai.ToolCall.ID in agreement.
func (a *callAssembler) synthesizedCallIDs() map[string]string {
	out := make(map[string]string, len(a.order))
	for i, itemID := range a.order {
		out[itemID] = synthesizedCallID(a.byID[itemID].ID, i)
	}
	return out
}

// synthesizedCallID returns callID unchanged when non-empty, else the same
// stable "call_N" placeholder calls()/synthesizedCallIDs() use.
func synthesizedCallID(callID string, index int) string {
	if callID != "" {
		return callID
	}
	return fmt.Sprintf("call_%d", index)
}

func usageFromWire(u *responseUsage) *ai.Usage {
	if u == nil {
		return nil
	}
	out := &ai.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// toolChoiceWire maps ai.ChatRequest.ToolChoice to the Responses API
// tool_choice shape: "" (adapter default) and "auto"/"none"/"required" pass
// through as bare strings, and any other value names a specific tool --
// {"type":"function","name":...}, flat (unlike Chat Completions' nested
// function object; see namedToolChoice).
func toolChoiceWire(choice string) any {
	switch choice {
	case "", ai.ToolChoiceAuto:
		return "auto"
	case ai.ToolChoiceNone:
		return "none"
	case ai.ToolChoiceRequired:
		return "required"
	default:
		return namedToolChoice{Type: "function", Name: choice}
	}
}

// yieldFatal yields the single fatal-pair event the LLMProvider contract
// requires and nothing else. The caller must return immediately afterward.
func yieldFatal(yield func(ai.Event, error) bool, e *ai.Error) {
	yield(ai.Event{Type: ai.EventError, Error: e}, e)
}

// toAIError normalises err to *ai.Error, mapping context cancellation to
// ErrCodeCanceled (never retryable) ahead of any other classification.
func toAIError(ctx context.Context, err error) *ai.Error {
	var aiErr *ai.Error
	if errors.As(err, &aiErr) {
		if ctx.Err() != nil {
			return &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()}
		}
		return aiErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &ai.Error{Code: ai.ErrCodeCanceled, Message: err.Error()}
	}
	return &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true}
}

// reasoningUnsupported reports whether this Provider has already learned
// (markReasoningUnsupported) that model rejects the top-level `reasoning`
// field.
func (p *Provider) reasoningUnsupported(model string) bool {
	p.noReasoningMu.Lock()
	defer p.noReasoningMu.Unlock()
	return p.noReasoning[model]
}

// markReasoningUnsupported records that model rejects `reasoning`, keyed by
// model (not the whole Provider) since a Provider can be reused across
// requests naming different models.
func (p *Provider) markReasoningUnsupported(model string) {
	p.noReasoningMu.Lock()
	defer p.noReasoningMu.Unlock()
	if p.noReasoning == nil {
		p.noReasoning = map[string]bool{}
	}
	p.noReasoning[model] = true
}

// encryptedContentUnsupported reports whether this Provider has already
// learned (markEncryptedContentUnsupported) that model rejects
// include:["reasoning.encrypted_content"].
func (p *Provider) encryptedContentUnsupported(model string) bool {
	p.noEncryptedContentMu.Lock()
	defer p.noEncryptedContentMu.Unlock()
	return p.noEncryptedContent[model]
}

// markEncryptedContentUnsupported records that model rejects
// include:["reasoning.encrypted_content"] (NB2, r2 review), keyed by model
// for the same reason as markReasoningUnsupported.
func (p *Provider) markEncryptedContentUnsupported(model string) {
	p.noEncryptedContentMu.Lock()
	defer p.noEncryptedContentMu.Unlock()
	if p.noEncryptedContent == nil {
		p.noEncryptedContent = map[string]bool{}
	}
	p.noEncryptedContent[model] = true
}

// isUnsupportedReasoningError reports whether err is a 400 (ai.ErrCodeInvalid)
// whose message NAMES the top-level `reasoning` PARAMETER specifically --
// see M2 in Stream. NM1 (r2 review): this used to match ANY 400 whose
// message merely contained the substring "reasoning", which also matched
// unrelated, non-parameter errors (e.g. "missing required 'reasoning'
// item") that have nothing to do with whether the field itself is
// supported -- misclassifying one of those would incorrectly disable
// `reasoning` for the model going forward. Narrowed to the two documented
// phrasings a parameter-rejection 400 actually uses.
func isUnsupportedReasoningError(err error) bool {
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		return false
	}
	lower := strings.ToLower(aiErr.Message)
	if strings.Contains(lower, "reasoning.effort") {
		return true
	}
	return strings.Contains(lower, "unsupported parameter") && strings.Contains(lower, "'reasoning")
}

// isUnsupportedEncryptedContentError reports whether err is a 400
// (ai.ErrCodeInvalid) whose message names encrypted reasoning content --
// see NB2 in Stream (e.g. "Encrypted content is not supported with this
// model").
func isUnsupportedEncryptedContentError(err error) bool {
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		return false
	}
	lower := strings.ToLower(aiErr.Message)
	return strings.Contains(lower, "encrypted content") || strings.Contains(lower, "encrypted_content")
}

// classifyStreamError maps a Responses API error `code` (from a
// response.failed response.error or a top-level `error` event) to an
// *ai.Error, falling back to message when code is empty/unrecognised.
func classifyStreamError(code, message string) *ai.Error {
	if message == "" {
		message = "openairesponses: stream error"
	}
	switch code {
	case "rate_limit_exceeded":
		return &ai.Error{Code: ai.ErrCodeRateLimited, Message: message, Retryable: true}
	case "insufficient_quota":
		return &ai.Error{Code: ai.ErrCodeQuota, Message: message}
	case "server_error":
		return &ai.Error{Code: ai.ErrCodeUpstream, Message: message, Retryable: true}
	default:
		return &ai.Error{Code: ai.ErrCodeUpstream, Message: message}
	}
}

// doRequest issues one attempt. lastAttempt tells it not to bother waiting
// out a 429's Retry-After delay when nothing will retry afterward anyway --
// that wait would only add latency to a request that's about to fail out to
// the caller regardless (mirrors ai/openaicompat.Provider.doRequest).
func (p *Provider) doRequest(ctx context.Context, payload []byte, lastAttempt bool) (*http.Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &ai.Error{Code: ai.ErrCodeCanceled, Message: err.Error()}
		}
		return nil, &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	aiErr := httpStatusError(resp.StatusCode, b)
	if resp.StatusCode == http.StatusTooManyRequests && !lastAttempt && aiErr.IsRetryable() {
		retry.WaitOnRetryAfter(ctx, resp.Header.Get("Retry-After"))
	}
	return nil, aiErr
}

// quotaIndicators are the OpenAI error type/code values that mean the
// account's quota/credit is exhausted rather than a transient rate limit --
// retrying doesn't help until the account is topped up.
var quotaIndicators = map[string]bool{
	"insufficient_quota": true,
}

func httpStatusError(status int, body []byte) *ai.Error {
	var apiErr apiErrorBody
	_ = json.Unmarshal(body, &apiErr)
	msg := apiErr.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(status)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &ai.Error{Code: ai.ErrCodeAuth, Message: msg}
	case status == http.StatusTooManyRequests:
		if quotaIndicators[apiErr.Error.Type] || quotaIndicators[apiErr.Error.Code] {
			return &ai.Error{Code: ai.ErrCodeQuota, Message: msg}
		}
		return &ai.Error{Code: ai.ErrCodeRateLimited, Message: msg, Retryable: true}
	case status >= 500:
		return &ai.Error{Code: ai.ErrCodeUpstream, Message: msg, Retryable: true}
	default:
		return &ai.Error{Code: ai.ErrCodeInvalid, Message: msg}
	}
}

// buildInstructions renders System plus the STATIC context blocks as the
// Responses API "instructions" field -- the same stable, cacheable prefix
// ai/openaicompat.buildMessages renders as a leading system message.
func buildInstructions(req ai.ChatRequest) string {
	var sys strings.Builder
	sys.WriteString(req.System)
	for _, b := range req.Context {
		if b.Kind != ai.ContextStatic {
			continue
		}
		if sys.Len() > 0 {
			sys.WriteString("\n\n")
		}
		if b.Name != "" {
			sys.WriteString("# " + b.Name + "\n")
		}
		sys.WriteString(b.Text)
	}
	return sys.String()
}

// buildInput renders req.Messages as Responses API input items: user/
// assistant message items, function_call items (one per ToolCall, carrying
// its call_id) for an assistant message that invoked tools, and
// function_call_output items for a RoleTool message's ToolResults. Dynamic
// context is spliced onto the last "user" message item's content, same rule
// as ai/openaicompat.buildMessages (see its M5 doc): never the literal last
// item, which in a multi-step tool-calling turn can be a function_call/
// function_call_output item by the time a later step re-renders history.
//
// B1 (r1 review): an assistant message AFTER the last genuine ai.RoleUser
// message (i.e. within the CURRENT tool-calling loop) that carries
// Message.ProviderState is replayed from those captured raw output items
// VERBATIM instead of being rebuilt from Text/ToolCalls -- mirroring
// ai/anthropic.buildMessages' current-loop-only ProviderState scoping (REQ:
// anthropic-thinking-block-replay). This is what lets a reasoning item's
// encrypted_content survive across the steps of one ai/agent.Loop run
// without ever enabling OpenAI's server-side Store. An assistant turn from
// an EARLIER loop always uses the legacy Text/ToolCalls reconstruction,
// even if it also carries a ProviderState (out of scope for this turn's
// reasoning continuity, same rationale as ai/anthropic).
func buildInput(req ai.ChatRequest) []inputItem {
	lastUserIdx := -1
	for i, m := range req.Messages {
		if m.Role == ai.RoleUser {
			lastUserIdx = i
		}
	}

	items := make([]inputItem, 0, len(req.Messages))
	for i, m := range req.Messages {
		if m.Role == ai.RoleTool {
			for _, r := range m.ToolResults {
				output := r.Content
				if r.IsError && !strings.HasPrefix(output, "Error:") {
					output = "Error: " + output
				}
				items = append(items, functionCallOutputItem(r.CallID, output))
			}
			continue
		}

		if len(m.ProviderState) > 0 && i > lastUserIdx {
			var replay []json.RawMessage
			if err := json.Unmarshal(m.ProviderState, &replay); err == nil && len(replay) > 0 {
				for _, raw := range replay {
					items = append(items, rawInputItem(raw))
				}
				continue
			}
		}

		if m.Text != "" || len(m.ToolCalls) == 0 {
			items = append(items, messageItem(string(m.Role), m.Text))
		}
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			items = append(items, functionCallItem(tc.ID, tc.Name, string(args)))
		}
	}

	if dyn := renderDynamic(req.Context); dyn != "" {
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].raw == nil && items[i].kind == "message" && items[i].role == "user" {
				items[i].content = dyn + items[i].content
				break
			}
		}
	}
	return items
}

// renderDynamic renders the dynamic context blocks as a clearly delimited
// block, or "" when there are none. Identical rendering to
// ai/openaicompat.renderDynamic.
func renderDynamic(blocks []ai.ContextBlock) string {
	var dyn strings.Builder
	for _, b := range blocks {
		if b.Kind != ai.ContextDynamic {
			continue
		}
		if dyn.Len() > 0 {
			dyn.WriteString("\n\n")
		}
		if b.Name != "" {
			dyn.WriteString("# " + b.Name + "\n")
		}
		dyn.WriteString(b.Text)
	}
	if dyn.Len() == 0 {
		return ""
	}
	return "[context]\n" + dyn.String() + "\n[/context]\n\n"
}

// extractJSON tolerates a ```json fenced response, returning the JSON body.
// Identical to ai/openaicompat.extractJSON.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}
