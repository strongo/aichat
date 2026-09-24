// Package anthropic is an ai.LLMProvider for the Anthropic Messages API,
// implemented directly over net/http -- no vendor SDK.
package anthropic

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
	"regexp"
	"strconv"
	"strings"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/internal/retry"
	"github.com/strongo/aichat/ai/internal/sse"
)

const (
	apiVersion   = "2023-06-01"
	defaultModel = "claude-haiku-4-5-20251001"
	defaultMax   = 2048
)

// Config configures a Provider. BaseURL and APIKey are required.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Headers    map[string]string
	HTTPClient *http.Client
}

// Provider implements ai.LLMProvider over the Anthropic Messages API.
type Provider struct {
	cfg Config
}

// New builds a Provider. It panics if BaseURL is empty.
func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		panic("anthropic: Config.BaseURL is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Provider{cfg: cfg}
}

// Name implements ai.LLMProvider.
func (p *Provider) Name() string { return "anthropic" }

// wire types

// contentBlock is the union of every Messages API content-block shape this
// adapter produces or consumes (text, tool_use, tool_result, thinking).
// Fields irrelevant to Type are omitted from the wire via omitempty.
type contentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result (Content, not Text: the wire field is "content")
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// thinking / redacted_thinking: captured verbatim off the stream into
	// Message.ProviderState and replayed unmodified by buildMessages — never
	// built directly from a ChatRequest. Signature/Data are opaque and MUST
	// travel byte-for-byte; the API 400s a tool-use continuation whose
	// thinking blocks were dropped or edited.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// MarshalJSON special-cases Type=="thinking": the "thinking"/"signature"
// keys MUST be present on the wire even when empty (a display:"omitted"
// thinking block has "" thinking text but is still a real block the API
// expects to see both keys on) — the default omitempty tags above are
// right for every OTHER block type (text/tool_use/tool_result/
// redacted_thinking), where an absent key is exactly what's wanted, so this
// override only fires for "thinking".
func (b contentBlock) MarshalJSON() ([]byte, error) {
	if b.Type != "thinking" {
		type alias contentBlock
		return json.Marshal(alias(b))
	}
	return json.Marshal(struct {
		Type         string        `json:"type"`
		Thinking     string        `json:"thinking"`
		Signature    string        `json:"signature"`
		CacheControl *cacheControl `json:"cache_control,omitempty"`
	}{Type: b.Type, Thinking: b.Thinking, Signature: b.Signature, CacheControl: b.CacheControl})
}

type cacheControl struct {
	Type string `json:"type"`
}

// providerStateBlock is the wire shape captured into Event.ProviderState /
// replayed from Message.ProviderState: ONE block of an assistant turn's
// FULL content array (text, thinking, redacted_thinking, tool_use —
// whatever the API actually returned, in stream order), byte-faithful. This
// is deliberately a SEPARATE type from contentBlock (used elsewhere for
// ordinary request-building, where omitempty is wanted): Thinking and
// Signature are NOT omitempty here, so a display:"omitted" thinking block's
// empty "" text still round-trips as an explicit empty string rather than
// silently vanishing from the replayed block.
type providerStateBlock struct {
	Type      string          `json:"type"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data,omitempty"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// knownProviderStateBlockTypes gates buildMessages' replay of an
// ai.Message.ProviderState it did not itself produce (e.g. one relayed
// through ai/cloud from an origin this adapter doesn't fully trust) to only
// the content-block types this Messages API integration understands.
var knownProviderStateBlockTypes = map[string]bool{
	"thinking":          true,
	"redacted_thinking": true,
	"text":              true,
	"tool_use":          true,
}

// wireMessage's Content is always the block-array form (never the bare
// string shorthand) so a cache_control marker on the last history message
// (see REQ: anthropic-history-cache) is uniform with every other message.
type wireMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toolChoiceWire: Type is "auto"|"any"|"none"|"tool"; Name is set only when
// Type is "tool".
type toolChoiceWire struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// thinkingConfig requests extended thinking. Type "adaptive" (Claude 4.6+:
// Opus 4.6/4.7/4.8/5/5.5, Sonnet 4.6/5, Fable 5/5.1) pairs with
// messagesRequestBody.OutputConfig.Effort and carries no BudgetTokens; type
// "enabled" (Haiku 4.5 and older) requires BudgetTokens and has no effort
// knob — see thinkingModeAdaptive.
type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// outputConfigWire carries the adaptive-thinking family's effort control.
type outputConfigWire struct {
	Effort string `json:"effort,omitempty"`
}

type messagesRequestBody struct {
	Model        string            `json:"model"`
	System       []contentBlock    `json:"system,omitempty"`
	Messages     []wireMessage     `json:"messages"`
	MaxTokens    int               `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	Tools        []toolDef         `json:"tools,omitempty"`
	ToolChoice   *toolChoiceWire   `json:"tool_choice,omitempty"`
	Thinking     *thinkingConfig   `json:"thinking,omitempty"`
	OutputConfig *outputConfigWire `json:"output_config,omitempty"`
}

// reasoningBudgets maps ai.ChatRequest.Reasoning levels to the legacy
// extended-thinking budget_tokens form (Haiku 4.5 and older models).
var reasoningBudgets = map[string]int{
	ai.ReasoningLow:    1024,
	ai.ReasoningMedium: 4096,
	ai.ReasoningHigh:   16000,
}

// modelFamilyRe extracts a Claude model id's family and version, tolerating
// a trailing dated snapshot suffix (e.g. "claude-haiku-4-5-20251001"): group
// 2/3 stop matching before it since FindStringSubmatch only anchors the
// leading "^claude-<family>-<major>[-<minor>]" shape.
var modelFamilyRe = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-(\d+)(?:-(\d+))?`)

// thinkingModeAdaptive reports whether model uses the current adaptive-
// thinking + output_config.effort form, versus the legacy
// thinking:{type:"enabled",budget_tokens:N} form. Per the claude-api skill
// (shared/model-migration.md, "Thinking & Effort" quick reference):
// Opus 4.6/4.7/4.8/5/5.5, Sonnet 4.6/5, and Fable 5/5.1 are adaptive-only
// (their thinking is on by default and rejects budget_tokens); Haiku 4.5
// and any pre-4.6 Sonnet/Opus still require budget_tokens. An id this
// adapter doesn't recognise (a future model not yet in that table) is
// assumed to be a newer, adaptive-thinking model — the same default the
// skill's own guidance uses for "unfamiliar model strings".
func thinkingModeAdaptive(model string) bool {
	// m4 (r2 review): Claude 3.x ids use the OLDER "claude-3[-<minor>]-
	// <family>" naming (claude-3-7-sonnet, claude-3-5-haiku, claude-3-opus,
	// claude-3-5-sonnet, claude-3-sonnet, ...) -- the family comes AFTER
	// the version, not before it like modelFamilyRe expects -- so they
	// never match that regex and would otherwise fall into the
	// "unrecognised model" default below (adaptive). That default is wrong
	// here: adaptive thinking did not exist for Claude 3.x at all, so
	// every claude-3-* id is legacy budget_tokens only.
	if model == "claude-3" || strings.HasPrefix(model, "claude-3-") {
		return false
	}
	m := modelFamilyRe.FindStringSubmatch(model)
	if m == nil {
		return true
	}
	family := m[1]
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3]) // "" -> 0, fine: no model has a bare "-4" version
	switch family {
	case "haiku":
		return false
	case "fable":
		return true
	case "opus", "sonnet":
		return major > 4 || (major == 4 && minor >= 6)
	default:
		return true
	}
}

// anthropicToolChoice maps ai.ChatRequest.ToolChoice to the Messages API
// tool_choice shape. Caveat: forced tool choice (ai.ToolChoiceRequired, or
// naming a specific tool) is REMOVED on the adaptive-only model family
// (Claude Fable 5.1, Claude Mythos 5.1, Claude Opus 5.5) -- the API 400s
// `{"type":"any"}`/`{"type":"tool",...}` there. Callers targeting one of
// those models must use ai.ToolChoiceAuto (plus an explicit prompt
// instruction naming the tool) instead; this adapter does not downgrade a
// forced choice for them automatically, since doing so silently would hide
// the caller's actual intent.
func anthropicToolChoice(choice string) *toolChoiceWire {
	switch choice {
	case "":
		return nil
	case ai.ToolChoiceAuto:
		return &toolChoiceWire{Type: "auto"}
	case ai.ToolChoiceNone:
		return &toolChoiceWire{Type: "none"}
	case ai.ToolChoiceRequired:
		return &toolChoiceWire{Type: "any"}
	default:
		return &toolChoiceWire{Type: "tool", Name: choice}
	}
}

type usagePayload struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type sseEvent struct {
	Type  string `json:"type"`
	Index *int   `json:"index,omitempty"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
		// Thinking/Signature: thinking_delta/signature_delta payloads.
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	} `json:"delta,omitempty"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		// Data: redacted_thinking's opaque payload, delivered whole (no
		// deltas follow).
		Data string `json:"data"`
	} `json:"content_block,omitempty"`
	Message *struct {
		Model string        `json:"model"`
		Usage *usagePayload `json:"usage"`
	} `json:"message,omitempty"`
	Usage *usagePayload `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Stream implements ai.LLMProvider.
//
// Fatal-error contract (see ai.LLMProvider doc): every fatal condition
// yields exactly one final (ai.Event{Type: ai.EventError, Error: e}, e) and
// returns; EventStarted is only yielded once the HTTP request has actually
// succeeded.
func (p *Provider) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		model := req.Model
		if model == "" || model == ai.ModelAuto {
			model = p.cfg.Model
		}
		if model == "" {
			model = defaultModel
		}
		maxTokens := req.MaxTokens
		if maxTokens == 0 {
			maxTokens = defaultMax
		}

		wantStructured := len(req.ResponseSchema) > 0
		body := messagesRequestBody{
			Model:     model,
			System:    buildSystemBlocks(req, wantStructured),
			Messages:  buildMessages(req),
			MaxTokens: maxTokens,
			Stream:    true,
		}
		// N3 remainder (r3 review): the adaptive-thinking family (Opus
		// 5/5.5, Sonnet 5, Fable, and other adaptive ids -- anything
		// thinkingModeAdaptive reports true for) thinks by default even
		// with Reasoning left UNSET -- omitting `thinking` does not
		// disable it on these models, it just leaves depth at the API's
		// own default. This must not be gated on `reasoningBudgets[req.
		// Reasoning]` (that map has no "" entry, so Reasoning:"" used to
		// skip the bump entirely and leave defaultMax's 2048, almost no
		// room to answer after reasoning). Apply it unconditionally
		// whenever the caller left MaxTokens unset (same M8 guard as
		// every other MaxTokens default here) and the model is adaptive.
		if req.MaxTokens == 0 && thinkingModeAdaptive(model) && body.MaxTokens < 16000 {
			body.MaxTokens = 16000
		}
		if len(req.Tools) > 0 {
			body.Tools = make([]toolDef, len(req.Tools))
			for i, t := range req.Tools {
				body.Tools[i] = toolDef{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
			}
			body.ToolChoice = anthropicToolChoice(req.ToolChoice)
		}
		// Reasoning -> thinking. Ruling (r1 review, M8): this adapter must
		// never raise MaxTokens above what the caller explicitly asked for
		// -- a caller-set ceiling is a hard budget, not a suggestion. Only
		// when the caller left MaxTokens unset (0, so maxTokens above is
		// already OUR default, not theirs) may we pick a larger default to
		// make room for a budget. Validating that a server-side minimum
		// MaxTokens is met is a separate, server-side follow-up -- not this
		// adapter's job.
		if budget, ok := reasoningBudgets[req.Reasoning]; ok {
			switch {
			case thinkingModeAdaptive(model):
				// Claude 4.6+ family: adaptive thinking, no budget_tokens;
				// depth is controlled by output_config.effort instead. The
				// N3 MaxTokens default for this family is applied
				// unconditionally above (regardless of Reasoning), not
				// here -- this switch only fires when req.Reasoning names
				// an actual budget level.
				body.Thinking = &thinkingConfig{Type: "adaptive"}
				body.OutputConfig = &outputConfigWire{Effort: req.Reasoning}
			case req.MaxTokens > 0 && req.MaxTokens < 2048:
				// Not enough room for a useful thinking budget (min 1024)
				// alongside any real output without exceeding the caller's
				// own MaxTokens: disable thinking rather than raise it.
			case req.MaxTokens > 0:
				// Caller set MaxTokens: shrink the budget (never MaxTokens)
				// to fit under it, floored at 1024.
				b := budget
				if b >= req.MaxTokens {
					b = req.MaxTokens - 1024
					if b < 1024 {
						b = 1024
					}
				}
				body.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: b}
			default:
				// Caller left MaxTokens unset (0): free to pick a larger
				// default so the budget has room -- this is OUR default,
				// not a ceiling the caller gave us. N3 (r2 review): budget
				// + 4096 (room for a real answer beyond the thinking
				// spend), capped at 16000 -- except the cap can never drop
				// AT or below budget itself (Anthropic requires
				// budget_tokens < max_tokens), so a "high" budget (16000)
				// still gets budget+1024 even though that's over the cap.
				mt := budget + 4096
				if mt > 16000 {
					mt = 16000
				}
				if mt <= budget {
					mt = budget + 1024
				}
				if mt > body.MaxTokens {
					body.MaxTokens = mt
				}
				body.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
			}
		}
		payload, err := json.Marshal(body)
		if err != nil {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeInvalid, Message: err.Error()})
			return
		}

		var resp *http.Response
		attempt := 0
		doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
			attempt++
			r, e := p.doRequest(ctx, payload, attempt >= retry.DefaultMaxAttempts)
			resp = r
			return e
		})
		if doErr != nil {
			yieldFatal(yield, toAIError(ctx, doErr))
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !yield(ai.Event{Type: ai.EventStarted, Provider: p.Name(), Model: model}, nil) {
			return
		}

		// Only accumulate the full text when a structured result must be
		// parsed from it; otherwise don't buffer the whole response (see
		// ai.LLMProvider doc: "must not buffer the full response").
		var textBuf strings.Builder
		var usage *ai.Usage
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		sc.Split(sse.ScanLines)
		var eventName string
		sawStop := false
		stopReasonWire := ""
		// toolCalls tracks tool_use blocks by index for EventToolCall
		// assembly (id/name at content_block_start, arguments concatenated
		// across input_json_delta chunks).
		toolCalls := map[int]*ai.ToolCall{}
		var toolOrder []int
		// blocks/blockOrder capture EVERY content block (text,
		// thinking/redacted_thinking, tool_use) verbatim and in stream
		// order, for Event.ProviderState (REQ: anthropic-thinking-block-
		// replay, B2): the entire assistant content array, byte-faithful,
		// so a later request replays it exactly rather than rebuilding it
		// from Text/ToolCalls (which would drop interleaved thinking).
		// thinking_delta/signature_delta are captured here but never
		// emitted as EventTextDelta.
		blocks := map[int]*providerStateBlock{}
		var blockOrder []int
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					continue
				}
				var se sseEvent
				if err := json.Unmarshal([]byte(data), &se); err != nil {
					yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: fmt.Sprintf("anthropic: bad event %q: %v", eventName, err)})
					return
				}
				typ := se.Type
				if typ == "" {
					typ = eventName
				}
				switch typ {
				case "message_start":
					if se.Message != nil && se.Message.Usage != nil {
						usage = mergeUsage(usage, toUsage(se.Message.Usage))
					}
				case "content_block_start":
					if se.Index != nil && se.ContentBlock != nil {
						idx := *se.Index
						blocks[idx] = &providerStateBlock{Type: se.ContentBlock.Type}
						blockOrder = append(blockOrder, idx)
						switch se.ContentBlock.Type {
						case "tool_use":
							toolCalls[idx] = &ai.ToolCall{ID: se.ContentBlock.ID, Name: se.ContentBlock.Name}
							toolOrder = append(toolOrder, idx)
							blocks[idx].ID = se.ContentBlock.ID
							blocks[idx].Name = se.ContentBlock.Name
						case "redacted_thinking":
							blocks[idx].Data = se.ContentBlock.Data
						}
					}
				case "content_block_delta":
					if se.Delta == nil {
						break
					}
					idx := 0
					if se.Index != nil {
						idx = *se.Index
					}
					switch se.Delta.Type {
					case "text_delta":
						if blk, ok := blocks[idx]; ok {
							blk.Text += se.Delta.Text
						}
						if se.Delta.Text == "" {
							break
						}
						if wantStructured {
							textBuf.WriteString(se.Delta.Text)
						}
						if !yield(ai.Event{Type: ai.EventTextDelta, Text: se.Delta.Text}, nil) {
							return
						}
					case "input_json_delta":
						if call, ok := toolCalls[idx]; ok && se.Delta.PartialJSON != "" {
							call.Arguments = append(call.Arguments, se.Delta.PartialJSON...)
						}
					case "thinking_delta":
						// Never emitted as text; captured for replay.
						if blk, ok := blocks[idx]; ok {
							blk.Thinking += se.Delta.Thinking
						}
					case "signature_delta":
						if blk, ok := blocks[idx]; ok {
							blk.Signature += se.Delta.Signature
						}
					}
				case "content_block_stop":
					// Nothing to do: tool_use calls are emitted together,
					// after message_stop, in stream order.
				case "message_delta":
					if se.Usage != nil {
						usage = mergeUsage(usage, toUsage(se.Usage))
						if !yield(ai.Event{Type: ai.EventUsage, Usage: usage}, nil) {
							return
						}
					}
					if se.Delta != nil && se.Delta.StopReason != "" {
						stopReasonWire = se.Delta.StopReason
					}
					if stopReasonWire == "max_tokens" && wantStructured {
						yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "anthropic: response truncated at max_tokens before a complete structured JSON object was produced"})
						return
					}
				case "message_stop":
					sawStop = true
				case "ping":
					// keepalive; no-op.
				case "error":
					msg := "anthropic error"
					if se.Error != nil {
						msg = se.Error.Message
					}
					yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: msg})
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			yieldFatal(yield, toAIError(ctx, err))
			return
		}
		if !sawStop {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "anthropic: stream truncated (no message_stop)"})
			return
		}

		if wantStructured && textBuf.Len() > 0 {
			raw := extractJSON(textBuf.String())
			if !yield(ai.Event{Type: ai.EventStructured, Structured: json.RawMessage(raw)}, nil) {
				return
			}
		}

		for _, idx := range toolOrder {
			call := *toolCalls[idx]
			if call.ID == "" {
				// m2: a malformed/absent id from the provider must not
				// reach callers as "" -- Handler dispatch and
				// ToolResult.CallID pairing both key off it.
				call.ID = fmt.Sprintf("call_%d", idx)
				if blk, ok := blocks[idx]; ok {
					blk.ID = call.ID // keep ProviderState's tool_use.id consistent
				}
			}
			if !yield(ai.Event{Type: ai.EventToolCall, ToolCall: &call}, nil) {
				return
			}
		}

		stopReason := ai.StopReasonEnd
		switch stopReasonWire {
		case "tool_use":
			stopReason = ai.StopReasonToolCalls
		case "max_tokens":
			stopReason = ai.StopReasonLength
		case "refusal":
			stopReason = ai.StopReasonRefusal
		case "pause_turn":
			stopReason = ai.StopReasonPauseTurn
		}
		var providerState json.RawMessage
		if len(blockOrder) > 0 {
			ordered := make([]providerStateBlock, 0, len(blockOrder))
			for _, idx := range blockOrder {
				blk := *blocks[idx]
				if call, ok := toolCalls[idx]; ok {
					// X1 (r2 review): a no-argument tool call streams zero
					// input_json_delta chunks, leaving call.Arguments empty.
					// A tool_use content block always carries "input" on
					// the wire (Anthropic requires the key even for an
					// empty object) -- capture it as "{}" here, not "",
					// so providerStateBlock's omitempty on Input doesn't
					// drop the key entirely and the replay below doesn't
					// need to special-case it either.
					input := call.Arguments
					if len(input) == 0 {
						input = json.RawMessage("{}")
					}
					blk.Input = input
				}
				ordered = append(ordered, blk)
			}
			if b, err := json.Marshal(ordered); err == nil {
				providerState = b
			}
		}
		yield(ai.Event{Type: ai.EventCompleted, Usage: usage, StopReason: stopReason, ProviderState: providerState}, nil)
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

// mergeUsage combines a running usage with a newer payload: message_delta
// only ever carries output_tokens (and sometimes nothing else), so a naive
// overwrite would clobber the input/cache figures message_start reported.
// Each field takes the newer NON-ZERO value, falling back to what was
// already known.
func mergeUsage(prev, next *ai.Usage) *ai.Usage {
	if prev == nil {
		return next
	}
	if next == nil {
		return prev
	}
	merged := *prev
	if next.InputTokens != 0 {
		merged.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		merged.OutputTokens = next.OutputTokens
	}
	if next.CacheReadTokens != 0 {
		merged.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheWriteTokens != 0 {
		merged.CacheWriteTokens = next.CacheWriteTokens
	}
	return &merged
}

func toUsage(u *usagePayload) *ai.Usage {
	return &ai.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// toolResultBlock renders one ai.ToolResult as a tool_result content block.
// IsError text is passed through unmodified — Anthropic's is_error flag
// carries the signal, not a text prefix.
func toolResultBlock(r ai.ToolResult) contentBlock {
	return contentBlock{Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError}
}

// messagesURL builds the Messages API URL, tolerating a BaseURL that
// already ends in "/v1" or "/v1/" -- ai/anthropic's request path is the
// fixed "/v1/messages", so a caller-supplied (or defaulted) BaseURL that
// already carries the "/v1" segment must have it stripped first, or the
// result doubles into ".../v1/v1/messages".
func messagesURL(base string) string {
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/messages"
}

// doRequest issues one attempt. lastAttempt tells it not to bother waiting
// out a 429's Retry-After delay when nothing will retry afterward anyway --
// that wait would only add latency to a request that's about to fail out to
// the caller regardless (m3).
func (p *Provider) doRequest(ctx context.Context, payload []byte, lastAttempt bool) (*http.Response, error) {
	url := messagesURL(p.cfg.BaseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("x-api-key", p.cfg.APIKey)
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

// isBillingError reports whether an Anthropic error body indicates
// exhausted credit/billing rather than a transient rate limit -- Anthropic
// has no single dedicated error type for this the way OpenAI's
// "insufficient_quota" is, so this checks both the "type" field and a
// message substring a low-balance response is documented to contain.
func isBillingError(errType, message string) bool {
	if strings.Contains(strings.ToLower(errType), "billing") {
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "credit balance") || strings.Contains(lower, "credit_balance")
}

func httpStatusError(status int, body []byte) *ai.Error {
	var apiErr struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
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
		if isBillingError(apiErr.Error.Type, apiErr.Error.Message) {
			return &ai.Error{Code: ai.ErrCodeQuota, Message: msg}
		}
		return &ai.Error{Code: ai.ErrCodeRateLimited, Message: msg, Retryable: true}
	case status >= 500:
		return &ai.Error{Code: ai.ErrCodeUpstream, Message: msg, Retryable: true}
	default:
		return &ai.Error{Code: ai.ErrCodeInvalid, Message: msg}
	}
}

// buildSystemBlocks renders System as a text block, then the STATIC context
// blocks in order with cache_control:ephemeral on the LAST static block
// (Anthropic prompt caching caches everything up to and including a marked
// block). Dynamic context is intentionally NOT rendered here -- it is
// per-turn data and must never sit ahead of (or inside) the cached, stable
// prefix; see buildMessages, which splices it into the current turn instead.
func buildSystemBlocks(req ai.ChatRequest, wantStructured bool) []contentBlock {
	var blocks []contentBlock
	if req.System != "" {
		blocks = append(blocks, contentBlock{Type: "text", Text: req.System})
	}
	lastStaticIdx := -1
	for _, b := range req.Context {
		if b.Kind != ai.ContextStatic {
			continue
		}
		text := b.Text
		if b.Name != "" {
			text = "# " + b.Name + "\n" + text
		}
		blocks = append(blocks, contentBlock{Type: "text", Text: text})
		lastStaticIdx = len(blocks) - 1
	}
	if lastStaticIdx >= 0 {
		blocks[lastStaticIdx].CacheControl = &cacheControl{Type: "ephemeral"}
	} else if len(blocks) > 0 {
		// No static context blocks: still worth caching the bare system
		// prompt if it's the only stable content.
		blocks[len(blocks)-1].CacheControl = &cacheControl{Type: "ephemeral"}
	}
	if wantStructured {
		blocks = append(blocks, contentBlock{
			Type: "text",
			Text: "Respond with ONLY a single JSON object matching this JSON Schema, no prose, no markdown fences:\n" + string(req.ResponseSchema),
		})
	}
	return blocks
}

// buildMessages renders the conversation history unchanged, then splices the
// DYNAMIC context blocks as a clearly delimited prefix of the LAST message
// (the current user turn) instead of the cached system prompt -- per-turn
// data must never precede or pollute the stable history. It also marks
// cache_control on the last message BEFORE the final turn (REQ:
// anthropic-history-cache), so the conversation history itself -- not just
// the system prompt -- is cached across turns.
func buildMessages(req ai.ChatRequest) []wireMessage {
	// N2 ruling (r2 review): ProviderState (thinking/redacted_thinking
	// blocks) is replayed ONLY for assistant turns AFTER the last genuine
	// user text message -- i.e. within the current tool-calling loop.
	// Earlier assistant turns (from a prior loop, or a prior conversation
	// turn) are rebuilt from Text/ToolCalls WITHOUT their thinking blocks
	// instead. This keeps a dynamic-context edit on a later user message
	// from ever landing ahead of (or "inside") an earlier turn's replayed
	// thinking block, and matches Anthropic's actual requirement, which is
	// scoped to the turn that led to the tool_use, not the whole history.
	lastUserIdx := -1
	for i, m := range req.Messages {
		if m.Role == ai.RoleUser {
			lastUserIdx = i
		}
	}

	msgs := make([]wireMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		if m.Role == ai.RoleTool {
			blocks := make([]contentBlock, 0, len(m.ToolResults))
			for _, r := range m.ToolResults {
				blocks = append(blocks, toolResultBlock(r))
			}
			// Consecutive RoleTool source messages merge into one "user"
			// message: Anthropic requires all tool_result blocks answering
			// one assistant turn to arrive together.
			if n := len(msgs); n > 0 && msgs[n-1].Role == "user" && isToolResultMessage(msgs[n-1]) {
				msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
				continue
			}
			msgs = append(msgs, wireMessage{Role: "user", Content: blocks})
			continue
		}

		var blocks []contentBlock
		// B2 (r1 review): when ProviderState is present AND this turn is
		// within the CURRENT tool-calling loop (N2 ruling: i > lastUserIdx,
		// see above), it IS the entire assistant content array (thinking/
		// redacted_thinking/text/tool_use, interleaved exactly as the API
		// returned them) — replay it VERBATIM rather than rebuilding from
		// Text/ToolCalls, which would drop interleaved thinking and reorder
		// blocks. thinking/redacted_thinking blocks in particular MUST
		// precede the tool_use they led to and MUST be replayed
		// byte-faithful (REQ: anthropic-thinking-block-replay) — the API
		// 400s a tool-use continuation whose thinking blocks were dropped
		// or edited. An assistant turn from an EARLIER loop (before the
		// last genuine user text message) falls to the legacy
		// Text/ToolCalls reconstruction below instead, even if it also
		// carries a ProviderState — Anthropic's replay requirement is
		// scoped to the turn that led to the CURRENT tool_use, not the
		// entire history, and rebuilding drops that older turn's thinking
		// blocks on purpose. m3: only known block types are accepted, so a
		// ProviderState relayed through an untrusted/foreign origin (e.g.
		// via ai/cloud) can't smuggle an unrecognised block onto the wire.
		// A ProviderState that fails to unmarshal, or unmarshals to zero
		// known blocks, also falls back to the legacy reconstruction
		// rather than sending an empty assistant turn.
		replayed := false
		if len(m.ProviderState) > 0 && i > lastUserIdx {
			var replay []providerStateBlock
			if err := json.Unmarshal(m.ProviderState, &replay); err == nil {
				for _, b := range replay {
					if !knownProviderStateBlockTypes[b.Type] {
						continue
					}
					// N1: never replay an empty text block onto the wire --
					// Anthropic rejects a "text" content block with no
					// text, so a captured-but-empty one (e.g. a step that
					// opened a text block and immediately stopped it) is
					// dropped rather than replayed.
					if b.Type == "text" && b.Text == "" {
						continue
					}
					// X1: defensive default for ProviderState captured
					// before this fix (or relayed from a foreign origin
					// via ai/cloud) that may still carry an empty/missing
					// input for a no-argument tool_use block.
					input := b.Input
					if b.Type == "tool_use" && len(input) == 0 {
						input = json.RawMessage("{}")
					}
					blocks = append(blocks, contentBlock{
						Type: b.Type, Text: b.Text, ID: b.ID, Name: b.Name, Input: input,
						Thinking: b.Thinking, Signature: b.Signature, Data: b.Data,
					})
				}
				replayed = len(blocks) > 0
			}
		}
		if !replayed {
			// N1: only emit a text block when there is actual text --
			// Anthropic rejects {"type":"text","text":""}. Previously this
			// also fired when m.Text=="" AND there were no ToolCalls
			// either, specifically to avoid sending a wire message with a
			// fully empty content array; that case is now handled by
			// skipping the message entirely below instead of padding it
			// with an empty text block.
			if m.Text != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, contentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
		}
		if len(blocks) == 0 {
			// N1: an assistant turn with no text, no tool calls, and no
			// (surviving) replayed content -- e.g. a refusal or an empty
			// end_turn that ai/agent.Loop still appended to a caller-built
			// transcript -- has nothing to say on the wire. Anthropic
			// rejects a message with an empty content array, and there is
			// nothing useful to pad it with, so the turn is dropped
			// entirely rather than sent.
			continue
		}
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: blocks})
	}

	// M5 (r1 review): dynamic context is spliced onto the LAST "user" TEXT
	// message specifically -- never the literal last wire message, which in
	// a multi-step tool-calling turn (ai/agent.Loop) is a merged
	// tool_result "user" message by the time a later step re-renders this
	// same history (Anthropic's tool_result blocks ALSO carry role:"user",
	// so a plain Role=="user" check isn't enough -- isToolResultMessage
	// excludes those). Splicing into a tool_result would corrupt it, and
	// changes what counts as "last" every step, breaking prompt-cache
	// stability turn to turn. With no genuine user text message at all,
	// dynamic context is dropped rather than corrupting whatever IS last.
	if dyn := renderDynamic(req.Context); dyn != "" {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != "user" || isToolResultMessage(msgs[i]) {
				continue
			}
			last := &msgs[i]
			if n := len(last.Content); n > 0 && last.Content[n-1].Type == "text" {
				last.Content[n-1].Text = dyn + last.Content[n-1].Text
			}
			break
		}
	}

	if len(msgs) >= 2 {
		histLast := &msgs[len(msgs)-2]
		if len(histLast.Content) > 0 {
			histLast.Content[len(histLast.Content)-1].CacheControl = &cacheControl{Type: "ephemeral"}
		}
	}
	return msgs
}

// isToolResultMessage reports whether every block of msg is a tool_result,
// i.e. it was built entirely from a RoleTool source message (never a mix —
// buildMessages never places tool_result blocks alongside other content).
func isToolResultMessage(msg wireMessage) bool {
	if len(msg.Content) == 0 {
		return false
	}
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// renderDynamic renders the dynamic context blocks as a clearly delimited
// block, or "" when there are none.
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

// extractJSON tolerates a ```json fenced response.
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
