// Package openaicompat is an ai.LLMProvider for the OpenAI Chat Completions
// streaming API (and any API-compatible endpoint), implemented directly over
// net/http -- no vendor SDK.
package openaicompat

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
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
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

type chatAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type chatChunk struct {
	Model   string        `json:"model"`
	Choices []chatChoice  `json:"choices"`
	Usage   *chatUsage    `json:"usage"`
	Error   *chatAPIError `json:"error,omitempty"`
}

type apiErrorBody struct {
	Error chatAPIError `json:"error"`
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

		body := chatRequestBody{
			Model:               model,
			Messages:            buildMessages(req),
			Stream:              true,
			StreamOptions:       &streamOptions{IncludeUsage: true},
			MaxCompletionTokens: req.MaxTokens,
		}
		wantStructured := len(req.ResponseSchema) > 0
		if wantStructured {
			strict := req.StrictSchema == nil || *req.StrictSchema
			body.ResponseFormat = &responseFormat{
				Type: "json_schema",
				JSONSchema: jsonSchemaFormat{
					Name:   "response",
					Schema: req.ResponseSchema,
					Strict: strict,
				},
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

		var structuredBuf strings.Builder
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		sc.Split(sse.ScanLines)
		var usage *ai.Usage
		sawDone := false
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				sawDone = true
				break
			}
			if data == "" {
				continue
			}
			var chunk chatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: fmt.Sprintf("openaicompat: bad chunk: %v", err)})
				return
			}
			if chunk.Error != nil {
				yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: chunk.Error.Message})
				return
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
				if c.FinishReason == "length" && wantStructured {
					yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "openaicompat: response truncated at max_completion_tokens before a complete structured JSON object was produced"})
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			yieldFatal(yield, toAIError(ctx, err))
			return
		}
		if !sawDone {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeUpstream, Message: "openaicompat: stream truncated (no [DONE] marker)"})
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

// doRequest issues one attempt. lastAttempt tells it not to bother waiting
// out a 429's Retry-After delay when nothing will retry afterward anyway --
// that wait would only add latency to a request that's about to fail out to
// the caller regardless (m3).
func (p *Provider) doRequest(ctx context.Context, payload []byte, lastAttempt bool) (*http.Response, error) {
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
// retrying doesn't help until the account is topped up, so these must not
// be marked Retryable even though they arrive on a 429.
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

// buildMessages renders System plus the STATIC context blocks as one leading
// system message (stable across turns, so a provider-side prefix cache can
// hit), then the conversation history unchanged, then splices the DYNAMIC
// context blocks as a clearly delimited prefix of the last message (assumed
// to be the current user turn) rather than into the cached system prefix --
// per-turn data must never precede/pollute the stable, cacheable history.
func buildMessages(req ai.ChatRequest) []chatMessage {
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

	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if sys.Len() > 0 {
		msgs = append(msgs, chatMessage{Role: "system", Content: sys.String()})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: string(m.Role), Content: m.Text})
	}

	if dyn := renderDynamic(req.Context); dyn != "" && len(msgs) > 0 {
		last := &msgs[len(msgs)-1]
		last.Content = dyn + last.Content
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
