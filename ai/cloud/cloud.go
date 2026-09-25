// Package cloud is the client for the cloudproto protocol (see
// ai/cloudproto): it implements ai.LLMProvider (chat) and exposes a separate
// decision.Provider via Decider(), plus a Usage lookup.
package cloud

import (
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
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/internal/retry"
)

// decisionTimeout is how long Chain should wait for the cloud decision
// service per call (see decision.Chain's optional DecisionTimeout hook). A
// remote decision call is inherently slower than a local rule/LLM call
// running against a nearby model, so it gets a longer allowance than
// Chain's 1500ms default.
const decisionTimeout = 4 * time.Second

// Config configures a Client.
type Config struct {
	// BaseURL is the API base URL including its version prefix, e.g.
	// "https://api.example.com/v0/". A trailing slash is added if missing.
	// The product supplies this (see ai/aiconfig); this package has no
	// default of its own.
	BaseURL string
	// Product identifies the consuming product for metering/limits/routing
	// and is sent as the X-AI-Product header and ai.ChatRequest.Product /
	// decision.Request.Product.
	Product string
	// ClientContext is included in both chat and decision requests unless
	// the caller provides more specific context for a particular turn.
	ClientContext *ai.ClientContext
	// Token returns the bearer token for each request.
	Token      func(context.Context) (string, error)
	HTTPClient *http.Client
}

// Client implements ai.LLMProvider (Name "cloud"). Its decision.Provider
// role is a SEPARATE value returned by Decider() (Name "cloud-decision"):
// Go dispatches one Name() per concrete type, so a single type cannot report
// two different names to two different interfaces, and Decider() is how
// that split is actually satisfied.
type Client struct {
	cfg Config
}

type interactionIDKey struct{}

// WithInteractionID associates requests made during one user turn. The ID
// remains client-supplied correlation metadata at the server trust boundary.
func WithInteractionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, interactionIDKey{}, id)
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

// Name implements ai.LLMProvider.
func (c *Client) Name() string { return "cloud" }

// Decider returns c's decision.Provider role (Name "cloud-decision"),
// backed by POST ai/decision. It also implements the optional
// `DecisionTimeout() time.Duration` interface decision.Chain honours, so a
// remote decision call gets more time than Chain's local-call default.
func (c *Client) Decider() decision.Provider { return decider{c} }

type decider struct{ c *Client }

func (d decider) Name() string { return "cloud-decision" }

func (d decider) DecisionTimeout() time.Duration { return decisionTimeout }

func (d decider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	return d.c.decide(ctx, req)
}

// Stream implements ai.LLMProvider by POSTing ai/chat and parsing the SSE
// response with cloudproto.ReadEvents.
//
// Fatal-error contract (see ai.LLMProvider doc): every fatal condition
// yields exactly one final (ai.Event{Type: ai.EventError, Error: e}, e) and
// returns; EventStarted is only yielded once the HTTP request has actually
// succeeded. cloudproto.ReadEvents already conforms (an EventError frame, a
// transport error, or an unterminated stream are all fatal there too), so
// Stream relays its events unchanged EXCEPT that a mid-stream failure caused
// by ctx being done (the body read was interrupted by cancellation, not a
// genuine transport/upstream fault) is remapped to ErrCodeCanceled here --
// cloudproto has no ctx of its own to make that distinction itself.
func (c *Client) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		if req.InteractionID == "" {
			req.InteractionID, _ = ctx.Value(interactionIDKey{}).(string)
		}
		if req.Product == "" {
			req.Product = c.cfg.Product
		}
		if req.ClientContext == nil {
			req.ClientContext = c.cfg.ClientContext
		}
		payload, err := json.Marshal(req)
		if err != nil {
			yieldFatal(yield, &ai.Error{Code: ai.ErrCodeInvalid, Message: err.Error()})
			return
		}

		var resp *http.Response
		doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
			r, e := c.doStreamRequest(ctx, payload)
			resp = r
			return e
		})
		if doErr != nil {
			yieldFatal(yield, toAIError(ctx, doErr))
			return
		}
		defer func() { _ = resp.Body.Close() }()

		for ev, err := range cloudproto.ReadEvents(resp.Body) {
			if err != nil && ctx.Err() != nil {
				yieldFatal(yield, &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()})
				return
			}
			if !yield(ev, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

func (c *Client) doStreamRequest(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := c.newRequest(ctx, cloudproto.PathChat, payload)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", cloudproto.ContentTypeSSE)
	resp, err := c.cfg.HTTPClient.Do(httpReq)
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
	return nil, decodeHTTPError(resp)
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

// decide implements the decision.Provider role.
func (c *Client) decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	if req.InteractionID == "" {
		req.InteractionID, _ = ctx.Value(interactionIDKey{}).(string)
	}
	if req.Product == "" {
		req.Product = c.cfg.Product
	}
	if req.ClientContext == nil {
		req.ClientContext = c.cfg.ClientContext
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return decision.Decision{}, false, err
	}

	var resp *http.Response
	doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
		httpReq, e := c.newRequest(ctx, cloudproto.PathDecision, payload)
		if e != nil {
			return e
		}
		httpReq.Header.Set("Accept", "application/json")
		r, e := c.cfg.HTTPClient.Do(httpReq)
		if e != nil {
			resp = nil
			if ctx.Err() != nil {
				return &ai.Error{Code: ai.ErrCodeCanceled, Message: e.Error()}
			}
			return &ai.Error{Code: ai.ErrCodeUpstream, Message: e.Error(), Retryable: true}
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			resp = r
			return nil
		}
		defer func() { _ = r.Body.Close() }()
		return decodeHTTPError(r)
	})
	if doErr != nil {
		return decision.Decision{}, false, toAIError(ctx, doErr)
	}
	defer func() { _ = resp.Body.Close() }()

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
	var resp *http.Response
	doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
		httpReq, e := c.newRequestMethod(ctx, http.MethodGet, cloudproto.PathUsage, nil)
		if e != nil {
			return e
		}
		httpReq.Header.Set("Accept", "application/json")
		r, e := c.cfg.HTTPClient.Do(httpReq)
		if e != nil {
			resp = nil
			if ctx.Err() != nil {
				return &ai.Error{Code: ai.ErrCodeCanceled, Message: e.Error()}
			}
			return &ai.Error{Code: ai.ErrCodeUpstream, Message: e.Error(), Retryable: true}
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			resp = r
			return nil
		}
		defer func() { _ = r.Body.Close() }()
		return decodeHTTPError(r)
	})
	if doErr != nil {
		return cloudproto.UsageResponse{}, toAIError(ctx, doErr)
	}
	defer func() { _ = resp.Body.Close() }()

	var ur cloudproto.UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return cloudproto.UsageResponse{}, fmt.Errorf("cloud: decode usage response: %w", err)
	}
	return ur, nil
}

// ReportInteraction sends one bounded, metadata-only client observation.
// Callers should invoke it after the user turn; a failure must not alter the
// result of an otherwise successful turn.
func (c *Client) ReportInteraction(ctx context.Context, report cloudproto.InteractionReport) error {
	if report.Product == "" {
		report.Product = c.cfg.Product
	}
	if report.ClientContext == nil {
		report.ClientContext = c.cfg.ClientContext
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, cloudproto.PathInteraction, payload)
	if err != nil {
		return err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("cloud: interaction report: HTTP %d", resp.StatusCode)
	}
	return nil
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
		// A token-source failure is an auth problem, not a transport one --
		// never retryable (retrying without fixing whatever broke the token
		// source would just fail again the same way).
		return nil, &ai.Error{Code: ai.ErrCodeAuth, Message: fmt.Sprintf("cloud: token: %v", err)}
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
		// The HTTP status is authoritative for retryability even when the
		// JSON body's own "retryable" was left false/omitted: a 429/5xx is
		// generally worth retrying before the first byte, regardless of
		// whether the server remembered to say so in the body -- EXCEPT
		// ErrCodeQuota, which a 429 sometimes carries for exhausted
		// allowance rather than a transient rate limit: retrying an
		// exhausted quota just fails again the same way, so it is never
		// forced retryable here.
		if e.Code != ai.ErrCodeQuota && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			e.Retryable = true
		}
		return &e
	}
	msg := nonJSONErrorMessage(resp, b)
	code := ai.ErrCodeUpstream
	retryable := resp.StatusCode >= 500
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		code = ai.ErrCodeAuth
	case http.StatusTooManyRequests:
		code, retryable = ai.ErrCodeRateLimited, true
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		code = ai.ErrCodeInvalid
	}
	return &ai.Error{Code: code, Message: msg, Retryable: retryable}
}

// maxNonJSONErrorExcerpt bounds how much of a non-JSON error body ever ends
// up in an *ai.Error.Message (and, from there, in a chat transcript or log
// line). A plain-text body that isn't JSON (and isn't HTML -- see
// nonJSONErrorMessage) is still short in practice (a load balancer's
// one-liner), but nothing upstream of this function guarantees that, so it
// is capped defensively rather than trusted.
const maxNonJSONErrorExcerpt = 200

// nonJSONErrorMessage builds the *ai.Error.Message for an error response
// whose body is not the expected cloudproto.ErrorResponse JSON (the
// decodeHTTPError caller already tried and failed that parse). The body
// commonly comes not from the product's own API but from an intermediary in
// front of it -- e.g. Cloudflare's own HTML 502/504 page when the origin
// times out or drops the connection before the application gets to write
// its JSON error body. Relaying that HTML verbatim into a chat transcript
// (as "upstream: <!DOCTYPE html>...") is useless to the person reading it
// and actively confusing (it reads as if the HTML markup IS the error), so
// any body that looks like HTML -- Content-Type: text/html, or a body whose
// first non-space byte is '<' as a defensive fallback for an intermediary
// that sends HTML without bothering to set the header -- collapses to one
// short, human-readable line instead. Anything else (a short plain-text
// body, or an empty one) keeps its previous fallback behaviour, just capped.
func nonJSONErrorMessage(resp *http.Response, body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if looksLikeHTML(resp, trimmed) {
		return fmt.Sprintf("AI service unavailable (HTTP %d %s) -- try again later", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	msg := string(trimmed)
	if msg == "" {
		return "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	if len(msg) > maxNonJSONErrorExcerpt {
		msg = msg[:maxNonJSONErrorExcerpt] + "…"
	}
	return msg
}

// looksLikeHTML reports whether an error body should be treated as an
// intermediary's HTML error page rather than a plain-text message from the
// product's own API.
func looksLikeHTML(resp *http.Response, trimmed []byte) bool {
	if ct := resp.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "text/html") {
		return true
	}
	return len(trimmed) > 0 && trimmed[0] == '<'
}
