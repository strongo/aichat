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

type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// wireMessage's Content is always the block-array form (never the bare
// string shorthand) so a cache_control marker on the last history message
// (see REQ: anthropic-history-cache) is uniform with every other message.
type wireMessage struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type messagesRequestBody struct {
	Model     string        `json:"model"`
	System    []textBlock   `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

type usagePayload struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type sseEvent struct {
	Type  string `json:"type"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta,omitempty"`
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
		payload, err := json.Marshal(body)
		if err != nil {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeInvalid, Message: err.Error()})
			return
		}

		var resp *http.Response
		doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
			r, e := p.doRequest(ctx, payload)
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
		sc.Split(scanSSELines)
		var eventName string
		sawStop := false
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
				case "content_block_delta":
					if se.Delta != nil && se.Delta.Type == "text_delta" && se.Delta.Text != "" {
						if wantStructured {
							textBuf.WriteString(se.Delta.Text)
						}
						if !yield(ai.Event{Type: ai.EventTextDelta, Text: se.Delta.Text}, nil) {
							return
						}
					}
				case "message_delta":
					if se.Usage != nil {
						usage = mergeUsage(usage, toUsage(se.Usage))
						if !yield(ai.Event{Type: ai.EventUsage, Usage: usage}, nil) {
							return
						}
					}
					if stopReason(data) == "max_tokens" && wantStructured {
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

		yield(ai.Event{Type: ai.EventCompleted, Usage: usage}, nil)
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

// stopReason extracts message_delta.delta.stop_reason without a dedicated
// struct field, since sseEvent's Delta shape is shared with
// content_block_delta and stop_reason only ever appears on message_delta.
func stopReason(rawData string) string {
	var v struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	_ = json.Unmarshal([]byte(rawData), &v)
	return v.Delta.StopReason
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

func (p *Provider) doRequest(ctx context.Context, payload []byte) (*http.Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/v1/messages"
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
	return nil, httpStatusError(resp.StatusCode, b)
}

func httpStatusError(status int, body []byte) error {
	var apiErr struct {
		Error struct {
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
func buildSystemBlocks(req ai.ChatRequest, wantStructured bool) []textBlock {
	var blocks []textBlock
	if req.System != "" {
		blocks = append(blocks, textBlock{Type: "text", Text: req.System})
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
		blocks = append(blocks, textBlock{Type: "text", Text: text})
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
		blocks = append(blocks, textBlock{
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
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: []textBlock{{Type: "text", Text: m.Text}}})
	}

	if dyn := renderDynamic(req.Context); dyn != "" && len(msgs) > 0 {
		last := &msgs[len(msgs)-1]
		if len(last.Content) > 0 {
			last.Content[len(last.Content)-1].Text = dyn + last.Content[len(last.Content)-1].Text
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

// scanSSELines is bufio.ScanLines but also splits on a bare '\r'.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
