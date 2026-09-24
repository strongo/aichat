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

type cacheControl struct {
	Type string `json:"type"`
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

// thinkingConfig requests extended thinking. BudgetTokens is required when
// Type is "enabled".
type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type messagesRequestBody struct {
	Model      string          `json:"model"`
	System     []contentBlock  `json:"system,omitempty"`
	Messages   []wireMessage   `json:"messages"`
	MaxTokens  int             `json:"max_tokens"`
	Stream     bool            `json:"stream"`
	Tools      []toolDef       `json:"tools,omitempty"`
	ToolChoice *toolChoiceWire `json:"tool_choice,omitempty"`
	Thinking   *thinkingConfig `json:"thinking,omitempty"`
}

// reasoningBudgets maps ai.ChatRequest.Reasoning levels to Anthropic
// extended-thinking budget_tokens.
var reasoningBudgets = map[string]int{
	ai.ReasoningLow:    1024,
	ai.ReasoningMedium: 4096,
	ai.ReasoningHigh:   16000,
}

// anthropicToolChoice maps ai.ChatRequest.ToolChoice to the Messages API
// tool_choice shape.
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
		if len(req.Tools) > 0 {
			body.Tools = make([]toolDef, len(req.Tools))
			for i, t := range req.Tools {
				body.Tools[i] = toolDef{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
			}
			body.ToolChoice = anthropicToolChoice(req.ToolChoice)
		}
		if budget, ok := reasoningBudgets[req.Reasoning]; ok {
			if body.MaxTokens <= budget {
				body.MaxTokens = budget + defaultMax
			}
			body.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
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
		// blockTypes/toolCalls track open content blocks by index so a
		// content_block_delta (which carries only the index, not the type)
		// can be routed to the right assembly (tool_use argument JSON is
		// streamed as input_json_delta chunks; thinking/signature deltas are
		// intentionally dropped — never emitted as text, per the tool-calling
		// contract).
		blockTypes := map[int]string{}
		toolCalls := map[int]*ai.ToolCall{}
		var toolOrder []int
		// reasoningBlocks/reasoningOrder capture thinking/redacted_thinking
		// blocks verbatim (text+signature, or the opaque redacted payload),
		// in stream order, for Event.ProviderState (see REQ:
		// anthropic-thinking-block-replay). They are never emitted as text.
		reasoningBlocks := map[int]*contentBlock{}
		var reasoningOrder []int
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
						blockTypes[idx] = se.ContentBlock.Type
						switch se.ContentBlock.Type {
						case "tool_use":
							toolCalls[idx] = &ai.ToolCall{ID: se.ContentBlock.ID, Name: se.ContentBlock.Name}
							toolOrder = append(toolOrder, idx)
						case "thinking":
							reasoningBlocks[idx] = &contentBlock{Type: "thinking"}
							reasoningOrder = append(reasoningOrder, idx)
						case "redacted_thinking":
							reasoningBlocks[idx] = &contentBlock{Type: "redacted_thinking", Data: se.ContentBlock.Data}
							reasoningOrder = append(reasoningOrder, idx)
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
						if blk, ok := reasoningBlocks[idx]; ok {
							blk.Thinking += se.Delta.Thinking
						}
					case "signature_delta":
						if blk, ok := reasoningBlocks[idx]; ok {
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
		}
		var providerState json.RawMessage
		if len(reasoningOrder) > 0 {
			blocks := make([]contentBlock, 0, len(reasoningOrder))
			for _, idx := range reasoningOrder {
				blocks = append(blocks, *reasoningBlocks[idx])
			}
			if b, err := json.Marshal(blocks); err == nil {
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
	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
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
		// Thinking/redacted_thinking blocks MUST precede text/tool_use in the
		// assistant turn that produced them, and MUST be replayed unmodified
		// (see REQ: anthropic-thinking-block-replay) — the API 400s a
		// tool-use continuation whose thinking blocks were dropped or
		// edited. m.ProviderState round-trips exactly what this adapter
		// itself captured off the stream (see Stream); an adapter-neutral
		// or foreign ProviderState that fails to unmarshal is dropped
		// rather than sent malformed.
		if len(m.ProviderState) > 0 {
			var reasoning []contentBlock
			if err := json.Unmarshal(m.ProviderState, &reasoning); err == nil {
				blocks = append(blocks, reasoning...)
			}
		}
		if m.Text != "" || len(m.ToolCalls) == 0 {
			blocks = append(blocks, contentBlock{Type: "text", Text: m.Text})
		}
		for _, tc := range m.ToolCalls {
			input := tc.Arguments
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, contentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
		}
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: blocks})
	}

	if dyn := renderDynamic(req.Context); dyn != "" && len(msgs) > 0 {
		last := &msgs[len(msgs)-1]
		if n := len(last.Content); n > 0 && last.Content[n-1].Type == "text" {
			last.Content[n-1].Text = dyn + last.Content[n-1].Text
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
