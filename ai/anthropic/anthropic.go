// Package anthropic is an ai.LLMProvider for the Anthropic Messages API,
// implemented directly over net/http -- no vendor SDK.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
			yield(ai.Event{}, err)
			return
		}

		var resp *http.Response
		doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
			r, e := p.doRequest(ctx, payload)
			resp = r
			return e
		})
		if doErr != nil {
			yield(ai.Event{}, doErr)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !yield(ai.Event{Type: ai.EventStarted, Provider: p.Name(), Model: model}, nil) {
			return
		}

		var textBuf strings.Builder
		var usage *ai.Usage
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var eventName string
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
					if !yield(ai.Event{}, fmt.Errorf("anthropic: bad event %q: %w", eventName, err)) {
						return
					}
					continue
				}
				typ := se.Type
				if typ == "" {
					typ = eventName
				}
				switch typ {
				case "message_start":
					if se.Message != nil && se.Message.Usage != nil {
						usage = toUsage(se.Message.Usage)
					}
				case "content_block_delta":
					if se.Delta != nil && se.Delta.Type == "text_delta" && se.Delta.Text != "" {
						textBuf.WriteString(se.Delta.Text)
						if !yield(ai.Event{Type: ai.EventTextDelta, Text: se.Delta.Text}, nil) {
							return
						}
					}
				case "message_delta":
					if se.Usage != nil {
						u := toUsage(se.Usage)
						if usage != nil {
							// message_delta usage carries only output tokens;
							// keep the cache/input figures from message_start.
							u.InputTokens = usage.InputTokens
							u.CacheReadTokens = usage.CacheReadTokens
							u.CacheWriteTokens = usage.CacheWriteTokens
						}
						usage = u
						if !yield(ai.Event{Type: ai.EventUsage, Usage: usage}, nil) {
							return
						}
					}
				case "message_stop":
					// handled after the loop
				case "error":
					msg := "anthropic error"
					if se.Error != nil {
						msg = se.Error.Message
					}
					aiErr := &ai.Error{Code: ai.ErrCodeUpstream, Message: msg}
					yield(ai.Event{Type: ai.EventError, Error: aiErr}, nil)
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			yield(ai.Event{}, err)
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

// buildSystemBlocks renders System as a text block, then the static context
// blocks in order with cache_control:ephemeral on the LAST static block
// (Anthropic prompt caching caches everything up to and including a marked
// block), then the dynamic context blocks as a following uncached block so
// the cached prefix never needs a per-turn value baked into it.
func buildSystemBlocks(req ai.ChatRequest, wantStructured bool) []textBlock {
	var blocks []textBlock
	if req.System != "" {
		blocks = append(blocks, textBlock{Type: "text", Text: req.System})
	}
	var lastStaticIdx = -1
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
	var dyn strings.Builder
	for _, b := range req.Context {
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
	if dyn.Len() > 0 {
		blocks = append(blocks, textBlock{Type: "text", Text: dyn.String()})
	}
	if wantStructured {
		blocks = append(blocks, textBlock{
			Type: "text",
			Text: "Respond with ONLY a single JSON object matching this JSON Schema, no prose, no markdown fences:\n" + string(req.ResponseSchema),
		})
	}
	return blocks
}

func buildMessages(req ai.ChatRequest) []wireMessage {
	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: m.Text})
	}
	return msgs
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
