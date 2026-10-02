// Package cloud is the client for the cloudproto protocol (see
// ai/cloudproto): it implements ai.LLMProvider (chat) and exposes a separate
// decision.Provider via Decider() that is also a decision.ScoredProvider (POST
// ai/score), plus a Usage lookup.
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
	"sync"
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
	// CapabilityTTL is how long the client remembers that the server has no ai/score
	// route (decision.ErrUnsupported) before it asks again: 0 means
	// DefaultCapabilityTTL, a negative value means until ResetCapabilities. A finite
	// TTL keeps one stray 404 during a rolling deploy from switching scoring off
	// until restart.
	CapabilityTTL time.Duration
	// MisconfiguredTTL is how long a "this base URL does not speak the protocol"
	// verdict (decision.ErrMisconfigured) is remembered, so a wrong base URL costs
	// one probe per TTL instead of two round trips per call, and stays loud: calls
	// in the window fail at once with the same error. 0 means
	// DefaultMisconfiguredTTL, a negative value means do not remember it.
	MisconfiguredTTL time.Duration
}

// Defaults of Config.CapabilityTTL and Config.MisconfiguredTTL.
const (
	DefaultCapabilityTTL    = 5 * time.Minute
	DefaultMisconfiguredTTL = 30 * time.Second
)

// Client implements ai.LLMProvider (Name "cloud"). Its decision.Provider
// role is a SEPARATE value returned by Decider() (Name "cloud-decision"):
// Go dispatches one Name() per concrete type, so a single type cannot report
// two different names to two different interfaces, and Decider() is how
// that split is actually satisfied.
type Client struct {
	cfg Config
	now func() time.Time // time.Now; tests replace it

	mu sync.Mutex // guards what follows
	// absent is set once the server is confirmed to have no ai/score route, until
	// absentUntil (the zero time: until ResetCapabilities).
	absent      bool
	absentUntil time.Time
	// misconfigured is the remembered "wrong base URL" error, until misconfUntil.
	misconfigured error
	misconfUntil  time.Time
	// probe is the GET ai/usage call in flight, shared by concurrent callers.
	probe *probeCall
}

// probeCall is one shared GET ai/usage probe: its callers wait on done.
type probeCall struct {
	done     chan struct{}
	err      error // the verdict every caller of the probe returns
	canceled bool  // the leader's own context ended, so followers may retry
}

// ResetCapabilities forgets what the client learned about the server: that it
// has no ai/score route, and that its base URL does not speak the protocol.
// Both verdicts expire on their own (Config.CapabilityTTL,
// Config.MisconfiguredTTL); call this after the server was upgraded or a base URL
// fixed to probe again at once.
func (c *Client) ResetCapabilities() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.absent, c.absentUntil, c.misconfigured, c.misconfUntil = false, time.Time{}, nil, time.Time{}
}

// verdict returns the remembered, still valid verdict about the server (the
// error a score call fails with at once), nil when there is none.
func (c *Client) verdict() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verdictLocked()
}

func (c *Client) verdictLocked() error {
	now := c.now()
	if c.absent {
		if c.absentUntil.IsZero() || now.Before(c.absentUntil) {
			return errScoreAbsent()
		}
		c.absent = false
	}
	if c.misconfigured != nil {
		if now.Before(c.misconfUntil) {
			return c.misconfigured
		}
		c.misconfigured = nil
	}
	return nil
}

func (c *Client) rememberAbsent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rememberAbsentLocked()
}

func (c *Client) rememberAbsentLocked() {
	c.absent, c.absentUntil = true, time.Time{}
	switch ttl := c.cfg.CapabilityTTL; {
	case ttl == 0:
		c.absentUntil = c.now().Add(DefaultCapabilityTTL)
	case ttl > 0:
		c.absentUntil = c.now().Add(ttl)
	}
}

func (c *Client) rememberMisconfiguredLocked(err error) {
	ttl := c.cfg.MisconfiguredTTL
	if ttl == 0 {
		ttl = DefaultMisconfiguredTTL
	}
	if ttl > 0 {
		c.misconfigured, c.misconfUntil = err, c.now().Add(ttl)
	}
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
	return &Client{cfg: cfg, now: time.Now}
}

// Name implements ai.LLMProvider.
func (c *Client) Name() string { return "cloud" }

// Decider returns c's decision.Provider role (Name "cloud-decision"),
// backed by POST ai/decision. The value is also a decision.ScoredProvider and a
// decision.TracedScorer backed by POST ai/score: a hosted decision endpoint can
// answer scored questions (table narrowing) too. Against a server that has no
// ai/score route, Score returns an error wrapping decision.ErrUnsupported, so a
// combinator moves on to its next engine. It also implements the optional
// `DecisionTimeout() time.Duration` interface decision.Chain honours, so a
// remote decision call gets more time than Chain's local-call default.
func (c *Client) Decider() decision.Provider { return decider{c} }

type decider struct{ c *Client }

var (
	_ decision.ScoredProvider = decider{}
	_ decision.TracedScorer   = decider{}
)

func (d decider) Name() string { return "cloud-decision" }

func (d decider) DecisionTimeout() time.Duration { return decisionTimeout }

func (d decider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	return d.c.decide(ctx, req)
}

// Score implements decision.ScoredProvider by POSTing ai/score.
func (d decider) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	res, _, err := d.c.score(ctx, req)
	return res, err
}

// ScoreTraced implements decision.TracedScorer: the report carries the
// strategy, answering engine and per-engine attempts the server reported.
func (d decider) ScoreTraced(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, decision.Report, error) {
	return d.c.score(ctx, req)
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

	resp, err := c.postJSON(ctx, cloudproto.PathDecision, payload, nil)
	if err != nil {
		return decision.Decision{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	var dr cloudproto.DecisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&dr); err != nil {
		return decision.Decision{}, false, fmt.Errorf("cloud: decode decision response: %w", err)
	}
	if !dr.Decided {
		return decision.Decision{}, false, nil
	}
	return remoteDecision(dr.Decision), true, nil
}

// remoteDecision is what a server's decision is trusted for. A server cannot
// make a decision deterministic: that class lives in an unexported field no JSON
// can set (decision.Deterministic), and an outcome of "deterministic" in the body
// is not honoured. Nor can it pre-judge an answer actionable: the Outcome of a
// remote decision is kept only when it is a refusal (uncertain, none, unscored,
// invalid), which only ever lowers what a caller does; every actionable claim is
// dropped, and the caller's own Chain or engine policy judges the answer. A server
// that wants a calibrated answer treated as one says Calibrated (with Scores); one
// that answers by exact rules is, for this client, a self-reported engine, and the
// product keeps its own rules in front of it (ai/decision/rules) when it needs a
// deterministic class. Protocol version 1 has no wire form for "deterministic".
func remoteDecision(d decision.Decision) decision.Decision {
	if d.Outcome.Actionable() {
		d.Outcome = ""
	}
	return d
}

// engineError is an error from a decision route (ai/decision, ai/score): the
// *ai.Error the server's answer decoded to, plus the engine-neutral sentinel it
// stands for (decision.ErrAuth, ErrInvalidRequest, ErrQuota, ErrMisconfigured; nil
// for a transient fault), so a circuit breaker and the combinators classify it the
// way they classify a typesafe error. errors.As still finds the *ai.Error.
type engineError struct {
	err  *ai.Error
	kind error
}

func (e *engineError) Error() string { return e.err.Error() }

func (e *engineError) Unwrap() []error {
	if e.kind == nil {
		return []error{e.err}
	}
	return []error{e.err, e.kind}
}

// RetryDelay implements decision.RetryDelayer.
func (e *engineError) RetryDelay() time.Duration { return e.err.RetryDelay() }

// IsRetryable: a response that asked callers to wait is not retried here after
// a few hundred milliseconds; the breaker and the combinators decide what to do
// with the delay.
func (e *engineError) IsRetryable() bool { return e.err.IsRetryable() && e.err.RetryAfterMs <= 0 }

// engineKind is the sentinel a decision-route error stands for, nil for a
// transient fault (rate limit, 5xx, transport).
func engineKind(status int, e *ai.Error) error {
	switch {
	case e.Code == ai.ErrCodeAuth || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return decision.ErrAuth
	case e.Code == ai.ErrCodeQuota:
		return decision.ErrQuota
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		// Not "route not implemented" (score handles that before this): an unknown
		// product, or a base URL that does not speak the protocol.
		return decision.ErrMisconfigured
	case e.Code == ai.ErrCodeInvalid || status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return decision.ErrInvalidRequest
	}
	return nil
}

// maxErrorBodyBytes bounds how much of an error body is read.
const maxErrorBodyBytes = 64 << 10

// postJSON POSTs payload to path and returns the 2xx response, retrying 429 and
// 5xx before the first byte like every other call (but not a response that asked
// callers to wait: see engineError). A non-2xx response is an engineError; first
// onStatus, when set, may claim it by returning a non-nil error (ai/score uses it
// for "route not implemented"). The body it receives is the (bounded) error body.
func (c *Client) postJSON(ctx context.Context, path string, payload []byte, onStatus func(ctx context.Context, status int, body []byte) error) (*http.Response, error) {
	var resp *http.Response
	doErr := retry.Do(ctx, retry.Config{}, func(ctx context.Context) error {
		httpReq, e := c.newRequest(ctx, path, payload)
		if e != nil {
			var aiErr *ai.Error
			if errors.As(e, &aiErr) {
				return &engineError{err: aiErr, kind: engineKind(0, aiErr)} // a token-source failure is auth
			}
			return e
		}
		httpReq.Header.Set("Accept", "application/json")
		r, e := c.cfg.HTTPClient.Do(httpReq)
		if e != nil {
			resp = nil
			if ctx.Err() != nil {
				return &ai.Error{Code: ai.ErrCodeCanceled, Message: e.Error()}
			}
			return &engineError{err: &ai.Error{Code: ai.ErrCodeUpstream, Message: e.Error(), Retryable: true}}
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			resp = r
			return nil
		}
		defer func() { _ = r.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxErrorBodyBytes))
		if onStatus != nil {
			if err := onStatus(ctx, r.StatusCode, body); err != nil {
				return err
			}
		}
		aiErr := decodeHTTPErrorBody(r, body)
		return &engineError{err: aiErr, kind: engineKind(r.StatusCode, aiErr)}
	})
	if doErr != nil {
		var ee *engineError
		if errors.Is(doErr, decision.ErrUnsupported) || errors.As(doErr, &ee) && ctx.Err() == nil {
			return nil, doErr
		}
		return nil, toAIError(ctx, doErr)
	}
	return resp, nil
}

// maxScoreResponseBytes bounds how much of an ai/score response is read.
const maxScoreResponseBytes = 4 << 20

// score implements POST ai/score. A request that fails decision.ValidateScoreRequest
// is refused locally (decision.ErrInvalidRequest, no call). A server that has no
// ai/score route gives decision.ErrUnsupported, remembered for the client's
// lifetime (see ResetCapabilities): a 501, or a 404/405 with a non-protocol body
// from a base URL that does answer GET ai/usage in the protocol. Any other
// 404/405 (a protocol ErrorResponse, or a base URL that does not speak the
// protocol at all) is decision.ErrMisconfigured. 401/403 and code "auth" are
// decision.ErrAuth, 400/422 and code "invalid" decision.ErrInvalidRequest, 429
// with code "quota" decision.ErrQuota; Retry-After is carried (ai.Error). The
// answers are validated against the request (decision.ValidateScoreResult) and an
// answer is Calibrated only when both the answer and the response say so.
func (c *Client) score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, decision.Report, error) {
	if err := decision.ValidateScoreRequest(req); err != nil {
		// Not %w of err: only its sentinel, the ids are the caller's.
		return decision.ScoreResult{}, decision.Report{}, fmt.Errorf("cloud: %w: the score request failed validation", decision.ErrInvalidRequest)
	}
	if err := c.verdict(); err != nil {
		return decision.ScoreResult{}, decision.Report{}, err
	}
	if req.Product == "" {
		req.Product = c.cfg.Product
	}
	interactionID, _ := ctx.Value(interactionIDKey{}).(string)
	payload, err := json.Marshal(cloudproto.ScoreRequest{ScoreRequest: req, InteractionID: interactionID, ClientContext: c.cfg.ClientContext})
	if err != nil {
		return decision.ScoreResult{}, decision.Report{}, fmt.Errorf("cloud: %w: the score request cannot be encoded", decision.ErrInvalidRequest)
	}
	resp, err := c.postJSON(ctx, cloudproto.PathScore, payload, c.scoreRouteAbsent)
	if err != nil {
		return decision.ScoreResult{}, decision.Report{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var sr cloudproto.ScoreResponse
	// The decode error is not wrapped: it can quote a fragment of the body.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxScoreResponseBytes)).Decode(&sr); err != nil {
		return decision.ScoreResult{}, decision.Report{}, errors.New("cloud: ai/score returned a malformed response")
	}
	res := decision.ScoreResult{Engine: sr.Engine, Model: sr.Model, Answers: map[string]decision.Answer{}}
	if res.Engine == "" {
		res.Engine = "cloud-decision"
	}
	if sr.Usage != nil {
		res.Usage = decision.Usage{InputTokens: int(sr.Usage.InputTokens), OutputTokens: int(sr.Usage.OutputTokens)}
	}
	for _, q := range req.Questions {
		a, ok := sr.Answers[q.ID]
		if !ok {
			continue // ValidateScoreResult reports the missing answer
		}
		// Sorted, and named by what the client asked, whatever the server sent.
		sorted := decision.NewAnswer(q.ID, q.Kind, a.Scores)
		sorted.Confidence, sorted.HasConfidence, sorted.NoneID = a.Confidence, a.HasConfidence, a.NoneID
		sorted.Calibrated = a.Calibrated && sr.Calibrated
		res.Answers[q.ID] = sorted
	}
	if err := decision.ValidateScoreResult(req, res); err != nil {
		return decision.ScoreResult{}, decision.Report{}, fmt.Errorf("cloud: ai/score returned an invalid result: %s", decision.InvalidDetail(err))
	}
	rep := decision.Report{Strategy: sr.Strategy, Engine: res.Engine, Attempts: sr.Attempts, Model: sr.Model}
	return res, rep, nil
}

// errScoreAbsent is the error for a server confirmed to have no ai/score route.
func errScoreAbsent() error {
	return fmt.Errorf("cloud: %s: the server has no such route: %w", cloudproto.PathScore, decision.ErrUnsupported)
}

// scoreRouteAbsent decides what a non-2xx answer from ai/score means before it
// is treated as an ordinary error. It returns nil for an ordinary error.
//
//   - 501: the route is not implemented, unambiguously.
//   - 404/405 whose body is a protocol ErrorResponse: an application error (an
//     unknown product), left to the ordinary mapping (a configuration error).
//   - any other 404/405: ambiguous, since an old server and a mistyped base URL
//     look the same. GET ai/usage settles it: a base URL that answers it in the
//     protocol lacks only ai/score (unsupported, remembered); one that does not is
//     misconfigured.
func (c *Client) scoreRouteAbsent(ctx context.Context, status int, body []byte) error {
	switch status {
	case http.StatusNotImplemented:
		c.rememberAbsent()
		return errScoreAbsent()
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		if isProtocolError(body) {
			return nil
		}
		return c.routeVerdict(ctx, status)
	}
	return nil
}

// isProtocolError reports whether body is a protocol ErrorResponse.
func isProtocolError(body []byte) bool {
	var er cloudproto.ErrorResponse
	return json.Unmarshal(body, &er) == nil && er.Error.Code != ""
}

// routeVerdict settles an ambiguous 404/405 from ai/score by asking GET ai/usage
// whether the base URL speaks the protocol, and returns the error the score call
// fails with: decision.ErrUnsupported when it does (the server only lacks ai/score;
// remembered for Config.CapabilityTTL), decision.ErrMisconfigured when it does not
// (the base URL is wrong; remembered for Config.MisconfiguredTTL), and an engine
// fault when the probe was inconclusive (a transport failure, a 429 or a 5xx
// without a protocol body; nothing remembered). A remembered verdict answers at
// once.
//
// Concurrent callers share ONE probe: the first is the leader, the others wait for
// its verdict, which it records atomically with finishing, so no caller can start a
// second probe for a question already answered. A follower whose leader was
// cancelled by the leader's own context probes again rather than inherit that
// cancellation.
func (c *Client) routeVerdict(ctx context.Context, status int) error {
	for {
		c.mu.Lock()
		if err := c.verdictLocked(); err != nil {
			c.mu.Unlock()
			return err
		}
		if call := c.probe; call != nil {
			c.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				return &ai.Error{Code: ai.ErrCodeCanceled, Message: ctx.Err().Error()}
			}
			if call.canceled && ctx.Err() == nil {
				continue
			}
			return call.err
		}
		call := &probeCall{done: make(chan struct{})}
		c.probe = call
		c.mu.Unlock()

		speaks, err := c.probeUsage(ctx)
		call.canceled = ctx.Err() != nil
		call.err = err
		if err == nil && !speaks {
			call.err = &engineError{
				err:  &ai.Error{Code: ai.ErrCodeInvalid, Message: fmt.Sprintf("cloud: %s answered HTTP %d and %s does not answer in the protocol either: the base URL is wrong", cloudproto.PathScore, status, cloudproto.PathUsage)},
				kind: decision.ErrMisconfigured,
			}
		}
		c.mu.Lock()
		c.probe = nil
		switch {
		case err != nil:
		case speaks:
			call.err = errScoreAbsent()
			c.rememberAbsentLocked()
		default:
			c.rememberMisconfiguredLocked(call.err)
		}
		c.mu.Unlock()
		close(call.done)
		return call.err
	}
}

// probeUsage is one GET ai/usage: whether it shows the base URL speaks the protocol:
// a 2xx JSON object of the shape of cloudproto.UsageResponse (a "product" string,
// and an "allowance" that is an object or null when present: any other JSON object,
// such as a catch-all 200 from a web host or a proxy, does not count), or any status
// with a protocol ErrorResponse (a 401 included), says yes; a 404 or other
// non-protocol answer says no. A transport failure, a 429 or a 5xx without a
// protocol body is inconclusive and returned as an error.
func (c *Client) probeUsage(ctx context.Context) (bool, error) {
	httpReq, err := c.newRequestMethod(ctx, http.MethodGet, cloudproto.PathUsage, nil)
	if err != nil {
		return false, c.engineErr(err)
	}
	httpReq.Header.Set("Accept", "application/json")
	r, err := c.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return false, &ai.Error{Code: ai.ErrCodeCanceled, Message: err.Error()}
		}
		return false, &engineError{err: &ai.Error{Code: ai.ErrCodeUpstream, Message: err.Error(), Retryable: true}}
	}
	defer func() { _ = r.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxErrorBodyBytes))
	switch {
	case r.StatusCode >= 200 && r.StatusCode < 300:
		return isUsageResponse(body), nil
	case isProtocolError(body):
		return true, nil
	case r.StatusCode == http.StatusTooManyRequests || r.StatusCode >= 500:
		aiErr := decodeHTTPErrorBody(r, body)
		return false, &engineError{err: aiErr, kind: engineKind(r.StatusCode, aiErr)}
	}
	return false, nil
}

// isUsageResponse reports whether body has the shape of cloudproto.UsageResponse:
// a JSON object with a non-empty "product" string and, when "allowance" is
// present, an object (or null).
func isUsageResponse(body []byte) bool {
	var obj struct {
		Product   any             `json:"product"`
		Allowance json.RawMessage `json:"allowance"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	if product, ok := obj.Product.(string); !ok || product == "" {
		return false
	}
	if len(obj.Allowance) > 0 {
		var al *ai.Allowance
		return json.Unmarshal(obj.Allowance, &al) == nil
	}
	return true
}

// engineErr wraps a token-source failure (an auth *ai.Error) as an engineError.
func (c *Client) engineErr(err error) error {
	var aiErr *ai.Error
	if errors.As(err, &aiErr) {
		return &engineError{err: aiErr, kind: engineKind(0, aiErr)}
	}
	return err
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
	httpReq.Header.Set(cloudproto.HeaderProtocol, strconv.Itoa(cloudproto.ProtocolVersion))
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
	return decodeHTTPErrorBody(resp, b)
}

// decodeHTTPErrorBody builds the *ai.Error for a non-2xx response whose body was
// already read, carrying the Retry-After it asked for (RetryAfterMs).
func decodeHTTPErrorBody(resp *http.Response, b []byte) *ai.Error {
	e := decodeErrorBody(resp, b)
	if d := decision.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); d > 0 {
		e.RetryAfterMs = max(e.RetryAfterMs, d.Milliseconds())
	}
	e.RetryAfterMs = min(e.RetryAfterMs, decision.MaxRetryDelay.Milliseconds())
	return e
}

func decodeErrorBody(resp *http.Response, b []byte) *ai.Error {
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
