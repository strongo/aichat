package openaicompat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/strongo/aichat/ai"
)

const preparedChatWireRevision = 1

// PreparedChatInfo describes the exact prepared Chat Completions body without
// exposing its prompt, credentials, or endpoint. It is not a cost bound.
type PreparedChatInfo struct {
	WireRevision     int
	Protocol         string
	Model            string
	BodySHA256       [sha256.Size]byte
	OutputTokenField OutputTokenField
	MaxOutputTokens  int
}

// PreparedChat is an opaque, single-use guarded request. Value copies share
// its private state and invocation lease; the zero value cannot send.
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

// PrepareGuarded serializes a concrete, text-only Chat Completions request
// before admission. It performs no network operation. The caller must prove
// any monetary bound and admission separately before invoking Stream.
func (p *Provider) PrepareGuarded(req ai.ChatRequest) (*PreparedChat, error) {
	if p == nil || p.cfg.Guarded == nil || p.cfg.HTTPClient == nil {
		return nil, invalidPrepared("guarded provider required")
	}
	guard := p.cfg.Guarded
	if guard.MaxOutboundAttempts != 1 || guard.MaxOutputTokens <= 0 || req.MaxTokens <= 0 || req.MaxTokens > guard.MaxOutputTokens {
		return nil, invalidPrepared("invalid guarded output or attempt limit")
	}
	if req.Model == "" || req.Model == ai.ModelAuto || strings.TrimSpace(req.Model) == "" {
		return nil, invalidPrepared("concrete model required")
	}
	field := p.cfg.OutputTokenField
	if field == "" {
		field = OutputTokenFieldMaxCompletionTokens
	}
	if field != OutputTokenFieldMaxCompletionTokens && field != OutputTokenFieldMaxTokens {
		return nil, invalidPrepared("invalid output token field")
	}
	if len(req.Tools) != 0 || req.ToolChoice != "" || len(req.ResponseSchema) != 0 || req.StrictSchema != nil || req.Reasoning != "" {
		return nil, invalidPrepared("unsupported non-text feature")
	}
	for _, message := range req.Messages {
		if (message.Role != ai.RoleUser && message.Role != ai.RoleAssistant) || len(message.ToolCalls) != 0 || len(message.ToolResults) != 0 || len(message.ProviderState) != 0 {
			return nil, invalidPrepared("unsupported message feature")
		}
	}
	endpoint := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, invalidPrepared("invalid endpoint")
	}
	body := chatRequestBody{
		Model:         req.Model,
		Messages:      buildMessages(req),
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if field == OutputTokenFieldMaxTokens {
		body.MaxTokens = req.MaxTokens
	} else {
		body.MaxCompletionTokens = req.MaxTokens
	}
	payload, err := p.marshalPrepared(body)
	if err != nil {
		return nil, invalidPrepared(fmt.Sprintf("invalid request body: %v", err))
	}
	headers := make(http.Header, len(p.cfg.Headers)+3)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		headers.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		headers.Set(k, v)
	}
	client := *p.cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &PreparedChat{state: &preparedChatState{
		info: PreparedChatInfo{
			WireRevision:     preparedChatWireRevision,
			Protocol:         "chat-completions",
			Model:            req.Model,
			BodySHA256:       sha256.Sum256(payload),
			OutputTokenField: field,
			MaxOutputTokens:  req.MaxTokens,
		},
		payload: payload, endpoint: endpoint, headers: headers, client: &client,
	}}, nil
}

// Info returns a copy of the prepared body's non-content metadata. A zero
// value PreparedChat returns zero metadata.
func (r *PreparedChat) Info() PreparedChatInfo {
	if r == nil || r.state == nil {
		return PreparedChatInfo{}
	}
	return r.state.info
}

// Stream makes at most one adapter-level HTTP POST, even when the exported
// wrapper was copied or the returned iterator is consumed repeatedly.
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
		streamResponse(ctx, yield, resp, "openai-compatible", state.info.Model, false)
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
	return &ai.Error{Code: ai.ErrCodeInvalid, Message: "openaicompat: " + message}
}
