package anthropic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/strongo/aichat/ai"
)

const (
	preparedChatWireRevision = 1
	// This exact adapter-supported wire subset is independent of the ordinary
	// Stream default; changing that default must not arm another guarded model.
	guardedTextModelHaiku45 = "claude-haiku-4-5-20251001"
)

// PreparedChatInfo is the non-content identity of a finalized Messages body.
// It is not evidence of a monetary bound or provider billing behavior.
type PreparedChatInfo = ai.PreparedChatInfo

// PreparedChat contains only a pointer to shared private state. Value copies
// share the same invocation lease, and the zero value cannot send.
type PreparedChat struct {
	state *preparedChatState
}

type preparedChatState struct {
	info     PreparedChatInfo
	payload  []byte
	endpoint string
	headers  http.Header
	client   *http.Client
	used     atomic.Bool
}

// PrepareGuardedChat offers the additive provider-neutral prepared contract.
func (p *Provider) PrepareGuardedChat(req ai.ChatRequest) (ai.PreparedChatCall, error) {
	return p.PrepareGuarded(req)
}

// PrepareGuarded freezes one text-only Messages POST without sending it.
// Admission and a proved monetary bound belong to the caller, not this method.
func (p *Provider) PrepareGuarded(req ai.ChatRequest) (*PreparedChat, error) {
	if p == nil || p.cfg.Guarded == nil || p.cfg.HTTPClient == nil {
		return nil, invalidPrepared("guarded provider required")
	}
	guard := p.cfg.Guarded
	if guard.MaxOutboundAttempts != 1 || guard.MaxOutputTokens <= 0 ||
		req.MaxTokens <= 0 || req.MaxTokens > guard.MaxOutputTokens {
		return nil, invalidPrepared("invalid guarded output or attempt limit")
	}
	if req.Model == "" || req.Model == ai.ModelAuto || strings.TrimSpace(req.Model) != req.Model ||
		!guard.TextModels[req.Model] || !guardedTextModelKnown(req.Model) || thinkingModeAdaptive(req.Model) {
		return nil, invalidPrepared("unsupported concrete text model")
	}
	// Any caller-supplied header could enable an unpriced beta or feature. The
	// guarded envelope is fixed to the four headers below.
	if len(p.cfg.Headers) != 0 {
		return nil, invalidPrepared("custom headers are unsupported")
	}
	if len(req.Messages) == 0 || len(req.Tools) != 0 || req.ToolChoice != "" ||
		len(req.ResponseSchema) != 0 || req.StrictSchema != nil || req.Reasoning != "" {
		return nil, invalidPrepared("unsupported non-text feature")
	}
	for _, block := range req.Context {
		if block.Kind != ai.ContextStatic && block.Kind != ai.ContextDynamic {
			return nil, invalidPrepared("unsupported context kind")
		}
	}
	for _, message := range req.Messages {
		if (message.Role != ai.RoleUser && message.Role != ai.RoleAssistant) ||
			strings.TrimSpace(message.Text) == "" || len(message.ToolCalls) != 0 ||
			len(message.ToolResults) != 0 || len(message.ProviderState) != 0 {
			return nil, invalidPrepared("unsupported message feature")
		}
	}
	endpoint := messagesURL(p.cfg.BaseURL)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, invalidPrepared("invalid endpoint")
	}
	body := messagesRequestBody{
		Model: req.Model, System: buildSystemBlocks(req, false),
		Messages: buildMessages(req), MaxTokens: req.MaxTokens, Stream: true,
	}
	// Ordinary Stream deliberately adds cache_control. This first guarded
	// subset does not include prompt-cache writes/reads in its chargeable set.
	for i := range body.System {
		body.System[i].CacheControl = nil
	}
	for i := range body.Messages {
		for j := range body.Messages[i].Content {
			body.Messages[i].Content[j].CacheControl = nil
		}
	}
	payload, err := p.marshalPrepared(body)
	if err != nil {
		return nil, invalidPrepared("invalid request body")
	}
	headers := make(http.Header, 4)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("anthropic-version", apiVersion)
	if p.cfg.APIKey != "" {
		headers.Set("x-api-key", p.cfg.APIKey)
	}
	client := *p.cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &PreparedChat{state: &preparedChatState{
		info: PreparedChatInfo{
			WireRevision: preparedChatWireRevision, Protocol: "anthropic-messages",
			Model: req.Model, BodySHA256: sha256.Sum256(payload),
			OutputTokenField: "max_tokens", MaxOutputTokens: req.MaxTokens,
		},
		payload: payload, endpoint: endpoint, headers: headers, client: &client,
	}}, nil
}

// guardedTextModelKnown is the adapter's exact supported wire subset, not a
// production route choice or a claim about live billing. An operator allowlist
// must independently include an ID before guarded preparation can use it.
func guardedTextModelKnown(model string) bool {
	return model == guardedTextModelHaiku45
}

// Info returns a value copy. Nil and zero values return empty metadata.
func (r *PreparedChat) Info() PreparedChatInfo {
	if r == nil || r.state == nil {
		return PreparedChatInfo{}
	}
	return r.state.info
}

// PreparedInfo implements the provider-neutral prepared-call contract.
func (r *PreparedChat) PreparedInfo() ai.PreparedChatInfo { return r.Info() }

// Stream invokes at most one adapter-level POST across all wrapper copies and
// iterator executions. A custom RoundTripper may have its own internal sends.
func (r *PreparedChat) Stream(ctx context.Context) iter.Seq2[ai.Event, error] {
	var state *preparedChatState
	if r != nil {
		state = r.state
	}
	return func(yield func(ai.Event, error) bool) {
		if state == nil {
			yieldFatal(yield, invalidPrepared("uninitialized prepared request"))
			return
		}
		if !state.used.CompareAndSwap(false, true) {
			yieldFatal(yield, invalidPrepared("prepared request already invoked"))
			return
		}
		if err := ctx.Err(); err != nil {
			yieldFatal(yield, toAIError(ctx, err))
			return
		}
		resp, err := state.send(ctx)
		if err != nil {
			yieldFatal(yield, toAIError(ctx, err))
			return
		}
		streamResponse(ctx, yield, resp, state.info.Model, false)
	}
}

func (s *preparedChatState) send(ctx context.Context) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(s.payload))
	if err != nil {
		return nil, err
	}
	req.Header = s.headers.Clone()
	resp, err := s.client.Do(req)
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
	body, _ := io.ReadAll(resp.Body)
	return nil, httpStatusError(resp.StatusCode, body)
}

func invalidPrepared(message string) *ai.Error {
	return &ai.Error{Code: ai.ErrCodeInvalid, Message: "anthropic: " + message}
}
