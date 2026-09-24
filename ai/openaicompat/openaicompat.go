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
	"sync"

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

	// noReasoningEffortMu guards noReasoningEffort.
	noReasoningEffortMu sync.Mutex
	// noReasoningEffort records, per MODEL (m3, r2 review — a Provider can
	// be reused across requests naming different models, e.g. via
	// ai.ChatRequest.Model, and whether reasoning_effort is accepted is a
	// property of the model/deployment, not the Provider as a whole), that
	// this Provider has learned, from a live 400 response, that endpoint
	// rejects the reasoning_effort field for that model -- some
	// OpenAI-compatible endpoints (proxies, older deployments, certain
	// third-party providers) 400 on an unknown parameter instead of
	// ignoring it. Once a model is recorded here, every later Stream call
	// for that model omits reasoning_effort from the start, rather than
	// paying the extra round trip on every request.
	noReasoningEffort map[string]bool
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
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireToolCall is both the request-side (assistant history) and the
// streamed-delta shape; Index is only present (and meaningful) in a delta.
type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type toolFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolDef struct {
	Type     string          `json:"type"`
	Function toolFunctionDef `json:"function"`
}

type namedToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
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
	Tools               []toolDef       `json:"tools,omitempty"`
	// ToolChoice is either a bare string ("auto"|"none"|"required") or a
	// namedToolChoice{"type":"function","function":{"name":...}}.
	ToolChoice      any    `json:"tool_choice,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type chatUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type chatDelta struct {
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
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
		if len(req.Tools) > 0 {
			body.Tools = make([]toolDef, len(req.Tools))
			for i, t := range req.Tools {
				body.Tools[i] = toolDef{
					Type: "function",
					Function: toolFunctionDef{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.Schema,
					},
				}
			}
			body.ToolChoice = toolChoiceWire(req.ToolChoice)
		}
		sentReasoningEffort := req.Reasoning != "" && !p.reasoningEffortUnsupported(model)
		if sentReasoningEffort {
			body.ReasoningEffort = req.Reasoning
		}
		payload, err := json.Marshal(body)
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
		if doErr != nil && sentReasoningEffort && isUnsupportedReasoningEffortError(doErr) {
			// M1 (r1 review): some OpenAI-compatible endpoints 400 on an
			// unrecognised reasoning_effort instead of ignoring it. Retry
			// ONCE, before any byte of a response was seen, without it --
			// and remember not to send it again on this Provider instance.
			p.markReasoningEffortUnsupported(model)
			body.ReasoningEffort = ""
			if retryPayload, merr := json.Marshal(body); merr == nil {
				resp, doErr = send(retryPayload)
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
		var usage *ai.Usage
		sawDone := false
		finishReason := ""
		asm := newToolCallAssembler()
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
				if chunk.Usage.CompletionTokensDetails != nil {
					u.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
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
				for _, tc := range c.Delta.ToolCalls {
					asm.addDelta(tc)
				}
				if c.FinishReason != "" {
					finishReason = c.FinishReason
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

		for _, call := range asm.calls() {
			c := call
			if !yield(ai.Event{Type: ai.EventToolCall, ToolCall: &c}, nil) {
				return
			}
		}

		stopReason := ai.StopReasonEnd
		switch finishReason {
		case "tool_calls":
			stopReason = ai.StopReasonToolCalls
		case "length":
			stopReason = ai.StopReasonLength
		}
		yield(ai.Event{Type: ai.EventCompleted, Usage: usage, StopReason: stopReason}, nil)
	}
}

// toolCallAssembler accumulates streamed delta.tool_calls chunks (id/name
// arrive on the first chunk for a given index, arguments arrive
// concatenated across subsequent chunks) into complete ai.ToolCall values,
// preserving index order.
type toolCallAssembler struct {
	order   []int
	byIndex map[int]*ai.ToolCall

	// M9: some OpenAI-compatible providers omit `index` on delta.tool_calls
	// chunks entirely. Without an index to key on, a new non-empty id that
	// differs from the call currently being assembled must start a NEW
	// call rather than silently merging into the previous one; a chunk
	// that repeats the same id (or omits it, continuing an in-progress
	// call's arguments) keeps appending to it. Indexed and non-indexed
	// calls share `order`/`byIndex` via synthetic negative keys so
	// interleaving with explicitly-indexed calls still assembles in
	// arrival order.
	haveNoIndex    bool
	lastNoIndexKey int
	lastNoIndexID  string
	noIndexNext    int
}

func newToolCallAssembler() *toolCallAssembler {
	return &toolCallAssembler{byIndex: map[int]*ai.ToolCall{}}
}

func (a *toolCallAssembler) addDelta(tc wireToolCall) {
	var idx int
	if tc.Index != nil {
		idx = *tc.Index
	} else if a.haveNoIndex && (tc.ID == "" || tc.ID == a.lastNoIndexID) {
		idx = a.lastNoIndexKey
	} else {
		a.noIndexNext--
		idx = a.noIndexNext
		a.haveNoIndex = true
		a.lastNoIndexKey = idx
		if tc.ID != "" {
			a.lastNoIndexID = tc.ID
		}
	}
	call, ok := a.byIndex[idx]
	if !ok {
		call = &ai.ToolCall{}
		a.byIndex[idx] = call
		a.order = append(a.order, idx)
	}
	if tc.ID != "" {
		call.ID = tc.ID
	}
	if tc.Function.Name != "" {
		call.Name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		call.Arguments = append(call.Arguments, tc.Function.Arguments...)
	}
}

func (a *toolCallAssembler) calls() []ai.ToolCall {
	out := make([]ai.ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := *a.byIndex[idx]
		if call.ID == "" {
			// m2: a malformed/absent id from the provider must not reach
			// callers as "" -- Handler dispatch and ToolResult.CallID
			// pairing both key off it. Synthesize a stable, unique one.
			call.ID = fmt.Sprintf("call_%d", idx)
		}
		out = append(out, call)
	}
	return out
}

// toolChoiceWire maps ai.ChatRequest.ToolChoice to the Chat Completions
// tool_choice shape: "" (adapter default, so omit — handled by the caller
// leaving it unset is not an option here since Tools is non-empty, so ""
// maps to "auto"), "auto"/"none"/"required" pass through as bare strings,
// and any other value names a specific tool.
func toolChoiceWire(choice string) any {
	switch choice {
	case "", ai.ToolChoiceAuto:
		return "auto"
	case ai.ToolChoiceNone:
		return "none"
	case ai.ToolChoiceRequired:
		return "required"
	default:
		nt := namedToolChoice{Type: "function"}
		nt.Function.Name = choice
		return nt
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

// isUnsupportedReasoningEffortError reports whether err is a 400
// (ai.ErrCodeInvalid) whose message specifically NAMES the reasoning_effort
// field — see M1 in Stream. m3 (r2 review): this used to also match the
// generic phrase "unsupported parameter", which is far too broad — it would
// trigger the retry-without-reasoning_effort path (and permanently disable
// it for the model) on a 400 caused by a completely unrelated unsupported
// field, discarding reasoning_effort for the wrong reason. Matching only on
// the field's own name (snake_case or camelCase, case-insensitive) is still
// a heuristic (providers don't standardise error message text), but it no
// longer fires on unrelated 400s.
func isUnsupportedReasoningEffortError(err error) bool {
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		return false
	}
	lower := strings.ToLower(aiErr.Message)
	return strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoningeffort")
}

// reasoningEffortUnsupported reports whether this Provider has already
// learned (markReasoningEffortUnsupported) that model rejects
// reasoning_effort.
func (p *Provider) reasoningEffortUnsupported(model string) bool {
	p.noReasoningEffortMu.Lock()
	defer p.noReasoningEffortMu.Unlock()
	return p.noReasoningEffort[model]
}

// markReasoningEffortUnsupported records that model rejects
// reasoning_effort, per m3 (r2 review): keyed by model, not the whole
// Provider, since a Provider can be reused across requests naming different
// models.
func (p *Provider) markReasoningEffortUnsupported(model string) {
	p.noReasoningEffortMu.Lock()
	defer p.noReasoningEffortMu.Unlock()
	if p.noReasoningEffort == nil {
		p.noReasoningEffort = map[string]bool{}
	}
	p.noReasoningEffort[model] = true
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
		if m.Role == ai.RoleTool {
			for _, r := range m.ToolResults {
				content := r.Content
				if r.IsError && !strings.HasPrefix(content, "Error:") {
					content = "Error: " + content
				}
				msgs = append(msgs, chatMessage{Role: "tool", Content: content, ToolCallID: r.CallID})
			}
			continue
		}
		wm := chatMessage{Role: string(m.Role), Content: m.Text}
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}") // m1: empty arguments -> "{}"
				}
				wm.ToolCalls[i] = wireToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: wireToolCallFunc{
						Name:      tc.Name,
						Arguments: string(args),
					},
				}
			}
		}
		msgs = append(msgs, wm)
	}

	// M5 (r1 review): dynamic context is spliced onto the LAST "user" TEXT
	// message specifically -- never the literal last wire message, which in
	// a multi-step tool-calling turn (ai/agent.Loop) is a "tool" message by
	// the time a later step re-renders this same history. Splicing into a
	// tool result would corrupt it; splicing changes what counts as "last"
	// every step, breaking prompt-cache stability turn to turn. With no
	// user message at all (unusual, but not impossible), dynamic context is
	// simply dropped rather than corrupting whatever IS last.
	if dyn := renderDynamic(req.Context); dyn != "" {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				msgs[i].Content = dyn + msgs[i].Content
				break
			}
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
