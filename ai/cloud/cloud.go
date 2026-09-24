// Package cloud is the client for the cloudproto protocol (see
// ai/cloudproto): it implements ai.LLMProvider (chat) and decision.Provider
// (decision), plus a Usage lookup.
package cloud

import (
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
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the API base URL including its version prefix, e.g.
	// "https://api.sneat.cloud/v0/". A trailing slash is added if missing.
	BaseURL string
	// Product identifies the consuming product for metering/limits/routing
	// and is sent as the X-AI-Product header and ai.ChatRequest.Product /
	// decision.Request.Product.
	Product string
	// Token returns the bearer token for each request.
	Token      func(context.Context) (string, error)
	HTTPClient *http.Client
}

// Client implements ai.LLMProvider (Name "cloud") and decision.Provider
// (Name "cloud-decision").
type Client struct {
	cfg Config
}

// New builds a Client. It panics if BaseURL or Token is unset, and
// normalises BaseURL to always end with "/".
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		panic("cloud: Config.BaseURL is required")
	}
	if cfg.Token == nil {
		panic("cloud: Config.Token is required")
	}
	if !strings.HasSuffix(cfg.BaseURL, "/") {
		cfg.BaseURL += "/"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Client{cfg: cfg}
}

// Name implements both ai.LLMProvider and decision.Provider. Go method sets
// cannot return a different string per interface on the same type (the
// brief's "Name cloud" / "Name cloud-decision" split is not literally
// satisfiable from one Name() method); *Client reports "cloud" for both
// roles. diag.Turn.Provider plus the fact that a Turn's Decision != nil
// already disambiguates which role produced a given diagnostic record.
const clientName = "cloud"

// Name implements ai.LLMProvider and decision.Provider.
func (c *Client) Name() string { return clientName }

// Stream implements ai.LLMProvider by POSTing ai/chat and parsing the SSE
// response with cloudproto.ReadEvents.
func (c *Client) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		if req.Product == "" {
			req.Product = c.cfg.Product
		}
		payload, err := json.Marshal(req)
		if err != nil {
			yield(ai.Event{}, err)
			return
		}
		httpReq, err := c.newRequest(ctx, cloudproto.PathChat, payload)
		if err != nil {
			yield(ai.Event{}, err)
			return
		}
		httpReq.Header.Set("Accept", cloudproto.ContentTypeSSE)

		resp, err := c.cfg.HTTPClient.Do(httpReq)
		if err != nil {
			yield(ai.Event{}, &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			yield(ai.Event{}, decodeHTTPError(resp))
			return
		}
		for ev, err := range cloudproto.ReadEvents(resp.Body) {
			if !yield(ev, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Decide implements decision.Provider.
func (c *Client) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	if req.Product == "" {
		req.Product = c.cfg.Product
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return decision.Decision{}, false, err
	}
	httpReq, err := c.newRequest(ctx, cloudproto.PathDecision, payload)
	if err != nil {
		return decision.Decision{}, false, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return decision.Decision{}, false, &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decision.Decision{}, false, decodeHTTPError(resp)
	}
	var dr cloudproto.DecisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return decision.Decision{}, false, fmt.Errorf("cloud: decode decision response: %w", err)
	}
	if !dr.Decided {
		return decision.Decision{}, false, nil
	}
	return dr.Decision, true, nil
}

// Usage calls GET ai/usage.
func (c *Client) Usage(ctx context.Context) (cloudproto.UsageResponse, error) {
	httpReq, err := c.newRequestMethod(ctx, http.MethodGet, cloudproto.PathUsage, nil)
	if err != nil {
		return cloudproto.UsageResponse{}, err
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return cloudproto.UsageResponse{}, &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudproto.UsageResponse{}, decodeHTTPError(resp)
	}
	var ur cloudproto.UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return cloudproto.UsageResponse{}, fmt.Errorf("cloud: decode usage response: %w", err)
	}
	return ur, nil
}

func (c *Client) newRequest(ctx context.Context, path string, payload []byte) (*http.Request, error) {
	return c.newRequestMethod(ctx, http.MethodPost, path, payload)
}

func (c *Client) newRequestMethod(ctx context.Context, method, path string, payload []byte) (*http.Request, error) {
	url := c.cfg.BaseURL + path
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set(cloudproto.HeaderProduct, c.cfg.Product)
	token, err := c.cfg.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloud: token: %w", err)
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	return httpReq, nil
}

func decodeHTTPError(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	var er cloudproto.ErrorResponse
	if err := json.Unmarshal(b, &er); err == nil && er.Error.Code != "" {
		e := er.Error
		return &e
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	code := ai.ErrCodeUpstream
	retryable := resp.StatusCode >= 500
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		code = ai.ErrCodeAuth
	case http.StatusTooManyRequests:
		code, retryable = ai.ErrCodeRateLimited, true
	case http.StatusBadRequest:
		code = ai.ErrCodeInvalid
	}
	return &ai.Error{Code: code, Message: msg, Retryable: retryable}
}
