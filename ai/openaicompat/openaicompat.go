// Package openaicompat is an ai.LLMProvider for the OpenAI Chat Completions
// streaming API (and any API-compatible endpoint), implemented directly over
// net/http -- no vendor SDK.
package openaicompat

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

// Config configures a Provider. BaseURL and APIKey are required; Model is the
// default used when a ChatRequest leaves Model empty or ai.ModelAuto.
type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Headers    map[string]string
	HTTPClient *http.Client
}

// Provider implements ai.LLMProvider over the Chat Completions API.
type Provider struct {
	cfg Config
}

// New builds a Provider. It panics if BaseURL is empty.
func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		panic("openaicompat: Config.BaseURL is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Provider{cfg: cfg}
}

// Name implements ai.LLMProvider.
func (p *Provider) Name() string { return "openai-compatible" }

// chat completion wire types (only the fields we use).

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type jsonSchemaFormat struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type responseFormat struct {
	Type       string           `json:"type"`
	JSONSchema jsonSchemaFormat `json:"json_schema"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequestBody struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type chatUsage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type chatDelta struct {
	Content string `json:"content"`
}

type chatChoice struct {
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type chatChunk struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Stream implements ai.LLMProvider.
func (p *Provider) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		model := req.Model
		if model == "" || model == ai.ModelAuto {
			model = p.cfg.Model
		}

		body := chatRequestBody{
			Model:         model,
			Messages:      buildMessages(req),
			Stream:        true,
			StreamOptions: &streamOptions{IncludeUsage: true},
			MaxTokens:     req.MaxTokens,
		}
		wantStructured := len(req.ResponseSchema) > 0
		if wantStructured {
			body.ResponseFormat = &responseFormat{
				Type: "json_schema",
				JSONSchema: jsonSchemaFormat{
					Name:   "response",
					Schema: req.ResponseSchema,
					Strict: true,
				},
			}
		}
		payload, err := json.Marshal(body)
		if err != nil {
			yield(ai.Event{}, err)
			return
		}

		var resp *http.Response
		doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
			r, e := p.doRequest(ctx, payload)
			if e != nil {
				resp = r
				return e
			}
			resp = r
			return nil
		})
		if doErr != nil {
			yield(ai.Event{}, doErr)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if !yield(ai.Event{Type: ai.EventStarted, Provider: p.Name(), Model: model}, nil) {
			return
		}

		var structuredBuf strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		var usage *ai.Usage
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			if data == "" {
				continue
			}
			var chunk chatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				if !yield(ai.Event{}, fmt.Errorf("openaicompat: bad chunk: %w", err)) {
					return
				}
				continue
			}
			if chunk.Usage != nil {
				u := &ai.Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
				if chunk.Usage.PromptTokensDetails != nil {
					u.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
				}
				usage = u
				if !yield(ai.Event{Type: ai.EventUsage, Usage: usage}, nil) {
					return
				}
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content != "" {
					if wantStructured {
						structuredBuf.WriteString(c.Delta.Content)
					}
					if !yield(ai.Event{Type: ai.EventTextDelta, Text: c.Delta.Content}, nil) {
						return
					}
				}
			}
		}
		if err := sc.Err(); err != nil {
			yield(ai.Event{}, err)
			return
		}

		if wantStructured && structuredBuf.Len() > 0 {
			raw := extractJSON(structuredBuf.String())
			if !yield(ai.Event{Type: ai.EventStructured, Structured: json.RawMessage(raw)}, nil) {
				return
			}
		}

		yield(ai.Event{Type: ai.EventCompleted, Usage: usage}, nil)
	}
}

func (p *Provider) doRequest(ctx context.Context, payload []byte) (*http.Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
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
		return &ai.Error{Code: ai.ErrCodeRateLimited, Message: msg, Retryable: true}
	case status >= 500:
		return &ai.Error{Code: ai.ErrCodeUpstream, Message: msg, Retryable: true}
	default:
		return &ai.Error{Code: ai.ErrCodeInvalid, Message: msg}
	}
}

// buildMessages renders System and Context as one leading system message
// (static blocks first, in order, then dynamic blocks), followed by the
// conversation. The Chat Completions API has no separate cache-control knob,
// so keeping the same leading text stable across turns is what lets a
// provider-side prefix cache (when the backend has one) still apply.
func buildMessages(req ai.ChatRequest) []chatMessage {
	var sys strings.Builder
	sys.WriteString(req.System)
	writeBlocks := func(kind ai.ContextKind) {
		for _, b := range req.Context {
			if b.Kind != kind {
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
	}
	writeBlocks(ai.ContextStatic)
	writeBlocks(ai.ContextDynamic)

	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if sys.Len() > 0 {
		msgs = append(msgs, chatMessage{Role: "system", Content: sys.String()})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: string(m.Role), Content: m.Text})
	}
	return msgs
}

// extractJSON tolerates a ```json fenced response, returning the JSON body.
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
