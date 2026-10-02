package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
)

// ---- a fake clock for breaker cooldowns ----

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock() *stepClock { return &stepClock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }
func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type noTimer struct{}

func (noTimer) Stop() bool                                         { return false }
func (c *stepClock) AfterFunc(time.Duration, func()) compose.Timer { return noTimer{} }

// ---- a scriptable server ----

// route scripts one path: a status, headers, a body.
type route struct {
	status  int
	headers map[string]string
	body    string
}

// protoServer serves the routes it is given (anything else answers 404 HTML) and
// counts hits per path.
type protoServer struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]route
	hits   map[string]int
	seen   http.Header
}

func newProtoServer(t *testing.T, routes map[string]route) *protoServer {
	t.Helper()
	p := &protoServer{routes: routes, hits: map[string]int{}}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v0/")
		p.mu.Lock()
		p.hits[path]++
		p.seen = r.Header.Clone()
		rt, ok := p.routes[path]
		p.mu.Unlock()
		if !ok {
			rt = route{status: 404, headers: map[string]string{"Content-Type": "text/html"}, body: "<html>proxy: not found SECRET-BODY</html>"}
		}
		for k, v := range rt.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rt.status)
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *protoServer) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[path]
}

func (p *protoServer) set(path string, r route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[path] = r
}

func (p *protoServer) client() *Client {
	return New(Config{BaseURL: p.URL + "/v0/", Product: "sneat", Token: tokenFunc("t")})
}

func apiError(code, msg string) string {
	b, _ := json.Marshal(cloudproto.ErrorResponse{Error: ai.Error{Code: code, Message: msg}})
	return string(b)
}

func jsonRoute(status int, body string, headers ...string) route {
	h := map[string]string{"Content-Type": "application/json"}
	for i := 0; i+1 < len(headers); i += 2 {
		h[headers[i]] = headers[i+1]
	}
	return route{status: status, headers: h, body: body}
}

var usageOK = jsonRoute(200, `{"product":"sneat"}`)

func decideReq() decision.Request {
	return decision.Request{Text: "hi", Taxonomy: decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "m", Intents: []string{"i"}}}}}
}

// ---- breaker classification on the cloud engine ----

// Five 400s or five 401s must not open the breaker, and must be recorded as
// "rejected" and "auth", on both decision routes. (The probed bug: *ai.Error
// matched neither sentinel, so every one counted as an engine failure.)
func TestCloud_CallerFaultsAreNeitherFailuresNorRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		status   int
		code     string
		sentinel error
		outcome  string
	}{
		"400 invalid": {400, "invalid", decision.ErrInvalidRequest, decision.AttemptRejected},
		"422 invalid": {422, "invalid", decision.ErrInvalidRequest, decision.AttemptRejected},
		"401 auth":    {401, "auth", decision.ErrAuth, decision.AttemptAuth},
		"403 auth":    {403, "auth", decision.ErrAuth, decision.AttemptAuth},
	} {
		for _, path := range []string{cloudproto.PathScore, cloudproto.PathDecision} {
			t.Run(name+" "+path, func(t *testing.T) {
				srv := newProtoServer(t, map[string]route{path: jsonRoute(tc.status, apiError(tc.code, "no"))})
				b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(2))
				e := compose.Single(b)
				var err error
				var rep decision.Report
				for i := 0; i < 6; i++ {
					if path == cloudproto.PathScore {
						_, rep, err = e.ScoreTraced(context.Background(), libraryScoreRequest())
					} else {
						_, _, rep, err = e.DecideTraced(context.Background(), decideReq())
					}
					if !errors.Is(err, tc.sentinel) {
						t.Fatalf("call %d: %v", i, err)
					}
				}
				var aiErr *ai.Error
				if !errors.As(err, &aiErr) || aiErr.Code != tc.code {
					t.Fatalf("the *ai.Error must stay reachable: %v", err)
				}
				if b.State() != compose.BreakerClosed {
					t.Fatalf("a caller fault opened the breaker: %v", b.State())
				}
				if rep.Attempts[0].Outcome != tc.outcome {
					t.Fatalf("attempt = %+v", rep.Attempts[0])
				}
				if got := srv.count(path); got != 6 {
					t.Fatalf("hits = %d: a 4xx is not retried", got)
				}
			})
		}
	}
}

// A token source that fails is the caller's configuration, not the engine's.
func TestCloud_TokenFailureIsAuthAndNeutral(t *testing.T) {
	c := New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: func(context.Context) (string, error) { return "", errors.New("no token") }})
	b := compose.NewBreaker(c.Decider(), compose.WithBreakerThreshold(1))
	for i := 0; i < 3; i++ {
		_, _, err := b.Decide(context.Background(), decideReq())
		if !errors.Is(err, decision.ErrAuth) {
			t.Fatalf("decide: %v", err)
		}
		if _, err := b.Score(context.Background(), libraryScoreRequest()); !errors.Is(err, decision.ErrAuth) {
			t.Fatalf("score: %v", err)
		}
	}
	if b.State() != compose.BreakerClosed {
		t.Fatal("opened")
	}
}

// ---- quota ----

func TestCloud_QuotaIsSurfacedNotRetriedNotFailedOverAndBreakerNeutral(t *testing.T) {
	quota := jsonRoute(429, apiError(ai.ErrCodeQuota, "allowance exhausted"))
	srv := newProtoServer(t, map[string]route{cloudproto.PathScore: quota})
	b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(1))
	backup := &countingScorer{stubScorer: stubScorer{name: "paid-backup"}}
	e := compose.Fallback(b, backup)
	for i := 1; i <= 4; i++ {
		_, rep, err := e.ScoreTraced(context.Background(), libraryScoreRequest())
		if !errors.Is(err, decision.ErrQuota) || rep.FallbackFired || rep.Attempts[0].Outcome != decision.AttemptQuota {
			t.Fatalf("call %d: err=%v rep=%+v", i, err, rep)
		}
		if srv.count(cloudproto.PathScore) != i {
			t.Fatalf("call %d: hits=%d (a quota refusal is not retried)", i, srv.count(cloudproto.PathScore))
		}
	}
	if backup.n.Load() != 0 || b.State() != compose.BreakerClosed {
		t.Fatalf("backup calls=%d breaker=%v", backup.n.Load(), b.State())
	}
	var aiErr *ai.Error
	_, _, err := e.ScoreTraced(context.Background(), libraryScoreRequest())
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeQuota {
		t.Fatalf("the quota *ai.Error stays reachable: %v", err)
	}
	// Opting in hands the call to the backup.
	res, rep, err := compose.Fallback(b, backup, compose.WithFallbackOn(compose.OnQuota)).ScoreTraced(context.Background(), libraryScoreRequest())
	if err != nil || res.Engine != "paid-backup" || !rep.FallbackFired {
		t.Fatalf("opt-in: res=%+v rep=%+v err=%v", res, rep, err)
	}
	// ai/decision maps the same way.
	srv.set(cloudproto.PathDecision, quota)
	if _, _, err := b.Decide(context.Background(), decideReq()); !errors.Is(err, decision.ErrQuota) {
		t.Fatalf("decide: %v", err)
	}
}

type countingScorer struct {
	stubScorer
	n atomic.Int32
}

func (c *countingScorer) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	c.n.Add(1)
	return c.stubScorer.Score(ctx, req)
}

// A plain rate limit is transient: it IS an engine-health failure, retried, and it
// opens the breaker.
func TestCloud_RateLimitIsAnEngineFailure(t *testing.T) {
	srv := newProtoServer(t, map[string]route{cloudproto.PathScore: jsonRoute(429, apiError(ai.ErrCodeRateLimited, "slow down"))})
	b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(2))
	for i := 0; i < 2; i++ {
		if _, err := b.Score(context.Background(), libraryScoreRequest()); err == nil || errors.Is(err, decision.ErrQuota) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.State() != compose.BreakerOpen || srv.count(cloudproto.PathScore) != 2*retryAttempts32() {
		t.Fatalf("state=%v hits=%d", b.State(), srv.count(cloudproto.PathScore))
	}
}

func retryAttempts32() int { return 3 }

// ---- Retry-After ----

func TestCloud_RetryAfterIsCarriedNotRetriedAndFloorsTheBreaker(t *testing.T) {
	for name, rt := range map[string]route{
		"seconds":         jsonRoute(429, apiError(ai.ErrCodeRateLimited, "slow"), "Retry-After", "90"),
		"http date":       jsonRoute(503, apiError(ai.ErrCodeUpstream, "down"), "Retry-After", time.Now().Add(95*time.Second).UTC().Format(http.TimeFormat)),
		"non-json 503":    {status: 503, headers: map[string]string{"Retry-After": "90"}, body: "busy"},
		"body field only": jsonRoute(503, `{"error":{"code":"upstream","message":"down","retryAfterMs":90000}}`),
		"header and body": jsonRoute(503, `{"error":{"code":"upstream","message":"down","retryAfterMs":1000}}`, "Retry-After", "90"),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newProtoServer(t, map[string]route{cloudproto.PathScore: rt})
			clk := newStepClock()
			b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(1), compose.WithBreakerCooldown(30*time.Second), compose.WithBreakerClock(clk))
			_, err := b.Score(context.Background(), libraryScoreRequest())
			if got := decision.RetryDelay(err); got < 85*time.Second || got > 95*time.Second {
				t.Fatalf("RetryDelay = %v (err %v)", got, err)
			}
			if srv.count(cloudproto.PathScore) != 1 {
				t.Fatalf("hits = %d: a response that asked callers to wait is not retried", srv.count(cloudproto.PathScore))
			}
			if b.State() != compose.BreakerOpen {
				t.Fatalf("state = %v", b.State())
			}
			clk.Advance(40 * time.Second) // past the cooldown, short of the Retry-After
			if _, err := b.Score(context.Background(), libraryScoreRequest()); !errors.Is(err, decision.ErrUnavailable) {
				t.Fatalf("Retry-After ignored by the cloud engine: %v", err)
			}
			clk.Advance(60 * time.Second)
			srv.set(cloudproto.PathScore, jsonRoute(200, `{}`)) // the probe goes out (and fails as malformed)
			if _, err := b.Score(context.Background(), libraryScoreRequest()); errors.Is(err, decision.ErrUnavailable) {
				t.Fatalf("no probe after the Retry-After: %v", err)
			}
		})
	}
}

func TestCloud_RetryAfterIsCappedAndIgnoresJunk(t *testing.T) {
	for header, want := range map[string]time.Duration{
		"100000000": decision.MaxRetryDelay,
		"-4":        0,
		"later":     0,
		"":          0,
		"0":         0,
		time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat): 0,
	} {
		srv := newProtoServer(t, map[string]route{cloudproto.PathDecision: jsonRoute(503, apiError(ai.ErrCodeUpstream, "x"), "Retry-After", header)})
		_, _, err := srv.client().Decider().Decide(context.Background(), decideReq())
		if got := decision.RetryDelay(err); got != want {
			t.Errorf("%q: %v, want %v", header, got, want)
		}
		srv.Close()
	}
	// A huge retryAfterMs in a body is capped too.
	srv := newProtoServer(t, map[string]route{cloudproto.PathDecision: jsonRoute(503, `{"error":{"code":"upstream","message":"x","retryAfterMs":9000000000}}`)})
	_, _, err := srv.client().Decider().Decide(context.Background(), decideReq())
	if got := decision.RetryDelay(err); got != decision.MaxRetryDelay {
		t.Errorf("body: %v", got)
	}
}

// ---- 404 ----

func scoreRoutes(score route, usage route) map[string]route {
	return map[string]route{cloudproto.PathScore: score, cloudproto.PathUsage: usage}
}

var (
	htmlNotFound = route{status: 404, headers: map[string]string{"Content-Type": "text/html"}, body: "<html>cloudflare 404 SECRET-BODY</html>"}
	plainMissing = route{status: 404, body: "404 page not found\n"}
)

// An old server (the protocol answers, ai/score does not exist) is unsupported,
// found out with ONE extra GET ai/usage, and remembered: later calls make no
// request at all, until ResetCapabilities.
func TestCloud_OldServerIsUnsupportedOnceAndRemembered(t *testing.T) {
	for name, miss := range map[string]route{"plain 404": plainMissing, "html 404": htmlNotFound, "405": {status: 405, body: "no"}, "501": {status: 501, body: ""}} {
		t.Run(name, func(t *testing.T) {
			srv := newProtoServer(t, scoreRoutes(miss, usageOK))
			c := srv.client()
			for i := 0; i < 5; i++ {
				_, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
				if !errors.Is(err, decision.ErrUnsupported) {
					t.Fatalf("call %d: %v", i, err)
				}
			}
			wantUsage := 1
			if miss.status == 501 {
				wantUsage = 0
			}
			if srv.count(cloudproto.PathScore) != 1 || srv.count(cloudproto.PathUsage) != wantUsage {
				t.Fatalf("score hits=%d usage hits=%d: the finding must be cached", srv.count(cloudproto.PathScore), srv.count(cloudproto.PathUsage))
			}
			// The server is upgraded; forgetting what was learned probes again.
			srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
			if _, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest()); !errors.Is(err, decision.ErrUnsupported) {
				t.Fatalf("still cached: %v", err)
			}
			c.ResetCapabilities()
			res, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
			if err != nil || res.Engine != "jev" {
				t.Fatalf("after reset: res=%+v err=%v", res, err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The probed bug: a mistyped base URL (everything answers 404, here from a proxy
// in front) used to look like "an old server" forever. It is now a loud
// configuration error that a Fallback does not hide behind its backup, that never
// opens a breaker, and that is not remembered (fix the URL and it works).
func TestCloud_MistypedBaseURLIsALoudConfigurationError(t *testing.T) {
	for name, miss := range map[string]route{"html": htmlNotFound, "plain": plainMissing} {
		t.Run(name, func(t *testing.T) {
			srv := newProtoServer(t, map[string]route{}) // everything 404s
			b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(1))
			backup := &countingScorer{stubScorer: stubScorer{name: "local"}}
			e := compose.Fallback(b, backup)
			srv.set(cloudproto.PathScore, miss)
			for i := 1; i <= 3; i++ {
				_, rep, err := e.ScoreTraced(context.Background(), libraryScoreRequest())
				if !errors.Is(err, decision.ErrMisconfigured) || errors.Is(err, decision.ErrUnsupported) || rep.FallbackFired || rep.Attempts[0].Outcome != decision.AttemptMisconfigured {
					t.Fatalf("call %d: err=%v rep=%+v", i, err, rep)
				}
				if strings.Contains(err.Error(), "SECRET-BODY") {
					t.Fatalf("the error quotes the body: %v", err)
				}
				if srv.count(cloudproto.PathScore) != i || srv.count(cloudproto.PathUsage) != i {
					t.Fatalf("call %d: score=%d usage=%d: nothing may be remembered", i, srv.count(cloudproto.PathScore), srv.count(cloudproto.PathUsage))
				}
			}
			if backup.n.Load() != 0 || b.State() != compose.BreakerClosed {
				t.Fatalf("backup=%d breaker=%v", backup.n.Load(), b.State())
			}
			// Fixing the URL (here: the routes appear) works without any reset.
			srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
			if res, err := b.Score(context.Background(), libraryScoreRequest()); err != nil || res.Engine != "jev" {
				t.Fatalf("fixed: %+v %v", res, err)
			}
		})
	}
}

// A JSON ErrorResponse 404 (an unknown product) is an application error: loud,
// and no probe is needed to say so.
func TestCloud_ProtocolErrorResponse404IsMisconfiguredWithoutAProbe(t *testing.T) {
	srv := newProtoServer(t, scoreRoutes(jsonRoute(404, apiError("invalid", "unknown product")), usageOK))
	_, err := srv.client().Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
	var aiErr *ai.Error
	if !errors.Is(err, decision.ErrMisconfigured) || errors.Is(err, decision.ErrUnsupported) || !errors.As(err, &aiErr) || srv.count(cloudproto.PathUsage) != 0 || srv.count(cloudproto.PathScore) != 1 {
		t.Fatalf("err=%v score=%d usage=%d", err, srv.count(cloudproto.PathScore), srv.count(cloudproto.PathUsage))
	}
	// A 405 with a protocol body is the same.
	srv.set(cloudproto.PathScore, jsonRoute(405, apiError("invalid", "wrong method")))
	if _, err := srv.client().Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest()); !errors.Is(err, decision.ErrMisconfigured) {
		t.Fatalf("405: %v", err)
	}
}

// What GET ai/usage must say for the base URL to count as speaking the protocol.
func TestCloud_ProbeVerdicts(t *testing.T) {
	for name, tc := range map[string]struct {
		usage       route
		wantUnsup   bool
		wantMisconf bool
		wantFault   bool
	}{
		"2xx json object":          {usage: usageOK, wantUnsup: true},
		"401 with a protocol body": {usage: jsonRoute(401, apiError("auth", "no")), wantUnsup: true},
		"2xx html catch-all":       {usage: route{status: 200, headers: map[string]string{"Content-Type": "text/html"}, body: "<html>spa</html>"}, wantMisconf: true},
		"2xx json null":            {usage: jsonRoute(200, `null`), wantMisconf: true},
		"404":                      {usage: htmlNotFound, wantMisconf: true},
		"401 without a body":       {usage: route{status: 401}, wantMisconf: true},
		"502 gateway":              {usage: route{status: 502, body: "bad gateway"}, wantFault: true},
		"429 without a body":       {usage: route{status: 429}, wantFault: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newProtoServer(t, scoreRoutes(plainMissing, tc.usage))
			c := srv.client()
			_, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
			switch {
			case tc.wantUnsup:
				if !errors.Is(err, decision.ErrUnsupported) || !c.scoreAbsent.Load() {
					t.Fatalf("err=%v", err)
				}
			case tc.wantMisconf:
				if !errors.Is(err, decision.ErrMisconfigured) || c.scoreAbsent.Load() {
					t.Fatalf("err=%v", err)
				}
			case tc.wantFault:
				// Inconclusive: an engine fault, nothing remembered, nothing unsupported.
				var aiErr *ai.Error
				if err == nil || errors.Is(err, decision.ErrUnsupported) || errors.Is(err, decision.ErrMisconfigured) || !errors.As(err, &aiErr) || c.scoreAbsent.Load() {
					t.Fatalf("err=%v", err)
				}
			}
		})
	}
}

func TestCloud_ProbeTransportFailureAndCancellationAndTokenFailure(t *testing.T) {
	// Score answers 404 and the probe cannot connect: inconclusive, a retryable fault.
	var n atomic.Int32
	c := New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, cloudproto.PathScore) {
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: http.NoBody}, nil
			}
			n.Add(1)
			return nil, errors.New("dial failed")
		})}})
	var aiErr *ai.Error
	_, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || errors.Is(err, decision.ErrUnsupported) || c.scoreAbsent.Load() {
		t.Fatalf("transport: %v", err)
	}
	// Cancelled while probing.
	ctx, cancel := context.WithCancel(context.Background())
	c = New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, cloudproto.PathScore) {
				return &http.Response{StatusCode: 404, Header: http.Header{}, Body: http.NoBody}, nil
			}
			cancel()
			return nil, ctx.Err()
		})}})
	if _, err := c.Decider().(decision.ScoredProvider).Score(ctx, libraryScoreRequest()); !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("cancelled: %v", err)
	}
	// The token source fails only at the probe (the score request got its token).
	var calls atomic.Int32
	c = New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat",
		Token: func(context.Context) (string, error) {
			if calls.Add(1) > 1 {
				return "", errors.New("token expired")
			}
			return "t", nil
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: http.NoBody}, nil
		})}})
	if _, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest()); !errors.Is(err, decision.ErrAuth) {
		t.Fatalf("probe token: %v", err)
	}
	// A bad URL makes the probe request itself fail to build.
	if err := New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: tokenFunc("t")}).engineErr(errors.New("plain")); err == nil || err.Error() != "plain" {
		t.Fatalf("engineErr = %v", err)
	}
}

func TestCloud_DecisionRoute404IsMisconfigured(t *testing.T) {
	srv := newProtoServer(t, map[string]route{}) // every route 404s
	b := compose.NewBreaker(srv.client().Decider(), compose.WithBreakerThreshold(1))
	for i := 0; i < 3; i++ {
		_, _, err := b.Decide(context.Background(), decideReq())
		if !errors.Is(err, decision.ErrMisconfigured) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.State() != compose.BreakerClosed {
		t.Fatal("opened")
	}
	// Through a Chain it is recorded distinctly.
	_, ok, tr := decision.Chain{Providers: []decision.Provider{srv.client().Decider()}}.Decide(context.Background(), decideReq())
	if ok || tr.Attempts[0].Outcome != decision.AttemptMisconfigured {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

// ---- protocol version, per-attempt usage and latency on the wire ----

func TestCloud_SendsTheProtocolVersionAndReadsPerAttemptUsage(t *testing.T) {
	resp := goodScoreResponse()
	resp.Protocol = 1
	resp.Strategy = "hedged"
	resp.Attempts = []decision.Attempt{
		{Provider: "jev", Outcome: decision.AttemptCancelled, Role: "primary", Latency: 1500 * time.Millisecond},
		{Provider: "llm", Outcome: decision.AttemptDecided, Role: "backup", Latency: 2300 * time.Millisecond, Usage: &decision.Usage{InputTokens: 400, OutputTokens: 80}},
	}
	srv := newProtoServer(t, map[string]route{cloudproto.PathScore: jsonRoute(200, mustJSON(t, resp))})
	_, rep, err := srv.client().Decider().(decision.TracedScorer).ScoreTraced(context.Background(), libraryScoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.seen.Get(cloudproto.HeaderProtocol); got != "1" {
		t.Fatalf("%s = %q", cloudproto.HeaderProtocol, got)
	}
	if len(rep.Attempts) != 2 || rep.Attempts[0].Usage != nil || rep.Attempts[1].Usage == nil || rep.Attempts[1].Usage.InputTokens != 400 ||
		rep.Attempts[1].Latency != 2300*time.Millisecond {
		t.Fatalf("attempts = %+v", rep.Attempts)
	}
	// The wire form: integer milliseconds, plus the legacy nanoseconds.
	if body := mustJSON(t, resp); !strings.Contains(body, `"latencyMs":2300`) || !strings.Contains(body, `"protocol":1`) || !strings.Contains(body, `"usage":{"inputTokens":400`) {
		t.Fatalf("wire = %s", body)
	}
}

// Per-attempt usage survives a compose engine: the cloud engine reports its own
// attempts, and a usage-less answer from a plain engine still gets one.
func TestCloud_UsageIsMeteredPerEngineThroughAFallback(t *testing.T) {
	srv := newProtoServer(t, map[string]route{cloudproto.PathScore: jsonRoute(200, mustJSON(t, goodScoreResponse()))})
	res, rep, err := compose.Fallback(srv.client().Decider(), stubScorer{name: "x"}).ScoreTraced(context.Background(), libraryScoreRequest())
	if err != nil || res.Usage.InputTokens != 300 || rep.Attempts[0].Provider != "jev" {
		t.Fatalf("res=%+v rep=%+v err=%v", res, rep, err)
	}
	if err := (&engineError{err: &ai.Error{Code: "x"}}).Unwrap(); len(err) != 1 {
		t.Fatal("a transient fault carries no sentinel")
	}
}
