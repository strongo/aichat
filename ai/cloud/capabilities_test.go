package cloud

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
)

// stubClock is a clock the test moves.
type stubClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStubClock() *stubClock { return &stubClock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }
func (c *stubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *stubClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (p *protoServer) clientAt(clk *stubClock, mutate func(*Config)) *Client {
	c := p.clientWith(mutate)
	c.now = clk.Now
	return c
}

func score(c *Client) error {
	_, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest())
	return err
}

// ---- m6: what counts as "speaks the protocol" ----

func TestCloud_ProbeRequiresTheUsageResponseShape(t *testing.T) {
	for name, tc := range map[string]struct {
		body      string
		wantUnsup bool
	}{
		"product only":            {`{"product":"sneat"}`, true},
		"with an allowance":       {`{"product":"sneat","allowance":{"unit":"tokens","used":1,"limit":9}}`, true},
		"null allowance":          {`{"product":"sneat","allowance":null}`, true},
		"unknown fields ignored":  {`{"product":"sneat","extra":[1]}`, true},
		"an unrelated object":     {`{"status":"ok","service":"spa"}`, false},
		"empty object":            {`{}`, false},
		"empty product":           {`{"product":""}`, false},
		"product not a string":    {`{"product":7}`, false},
		"allowance not an object": {`{"product":"sneat","allowance":[1]}`, false},
		"allowance a number":      {`{"product":"sneat","allowance":5}`, false},
		"array":                   {`[{"product":"sneat"}]`, false},
		"not JSON":                {`ok`, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newProtoServer(t, scoreRoutes(plainMissing, jsonRoute(200, tc.body)))
			err := score(srv.client())
			if tc.wantUnsup != errors.Is(err, decision.ErrUnsupported) || tc.wantUnsup == errors.Is(err, decision.ErrMisconfigured) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// ---- m6: the verdicts expire ----

func TestCloud_UnsupportedVerdictExpires(t *testing.T) {
	srv := newProtoServer(t, scoreRoutes(plainMissing, usageOK))
	clk := newStubClock()
	c := srv.clientAt(clk, func(*Config) {})
	if err := score(c); !errors.Is(err, decision.ErrUnsupported) || srv.count(cloudproto.PathScore) != 1 || srv.count(cloudproto.PathUsage) != 1 {
		t.Fatalf("err=%v", err)
	}
	// Remembered: no round trips at all within the default TTL.
	clk.Advance(DefaultCapabilityTTL - time.Second)
	if err := score(c); !errors.Is(err, decision.ErrUnsupported) || srv.count(cloudproto.PathScore) != 1 || srv.count(cloudproto.PathUsage) != 1 {
		t.Fatalf("err=%v score=%d usage=%d", err, srv.count(cloudproto.PathScore), srv.count(cloudproto.PathUsage))
	}
	// A rolling deploy finished: after the TTL the route is asked for again and works.
	srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
	clk.Advance(2 * time.Second)
	if err := score(c); err != nil || srv.count(cloudproto.PathScore) != 2 {
		t.Fatalf("after the TTL: err=%v", err)
	}
	if err := score(c); err != nil {
		t.Fatal(err)
	}
}

func TestCloud_CapabilityTTLIsConfigurableAndNegativeMeansForever(t *testing.T) {
	srv := newProtoServer(t, scoreRoutes(jsonRoute(501, apiError("upstream", "no route")), usageOK))
	clk := newStubClock()
	c := srv.clientAt(clk, func(c *Config) { c.CapabilityTTL = time.Minute })
	if err := score(c); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
	if err := score(c); err != nil {
		t.Fatalf("a 501 verdict also expires: %v", err)
	}

	srv.set(cloudproto.PathScore, plainMissing)
	forever := srv.clientAt(clk, func(c *Config) { c.CapabilityTTL = -1 })
	if err := score(forever); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatal(err)
	}
	clk.Advance(1000 * time.Hour)
	srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
	if err := score(forever); !errors.Is(err, decision.ErrUnsupported) {
		t.Fatalf("a negative TTL remembers until ResetCapabilities: %v", err)
	}
	forever.ResetCapabilities()
	if err := score(forever); err != nil {
		t.Fatalf("after ResetCapabilities: %v", err)
	}
}

func TestCloud_MisconfiguredVerdictIsRemembered(t *testing.T) {
	srv := newProtoServer(t, map[string]route{}) // everything 404s: a wrong base URL
	srv.set(cloudproto.PathScore, htmlNotFound)
	clk := newStubClock()
	c := srv.clientAt(clk, func(*Config) {})
	for i := 1; i <= 3; i++ {
		err := score(c)
		if !errors.Is(err, decision.ErrMisconfigured) || errors.Is(err, decision.ErrUnsupported) || strings.Contains(err.Error(), "SECRET-BODY") {
			t.Fatalf("call %d: loud and misconfigured: %v", i, err)
		}
		// Two round trips once, then none: a wrong base URL does not cost two per call.
		if srv.count(cloudproto.PathScore) != 1 || srv.count(cloudproto.PathUsage) != 1 {
			t.Fatalf("call %d: score=%d usage=%d", i, srv.count(cloudproto.PathScore), srv.count(cloudproto.PathUsage))
		}
	}
	// The TTL is short: the URL may have been fixed.
	clk.Advance(DefaultMisconfiguredTTL + time.Second)
	srv.set(cloudproto.PathScore, jsonRoute(200, mustJSON(t, goodScoreResponse())))
	if err := score(c); err != nil {
		t.Fatalf("fixed: %v", err)
	}
	// ResetCapabilities forgets it at once.
	srv.set(cloudproto.PathScore, htmlNotFound)
	c2 := srv.clientAt(clk, func(c *Config) { c.MisconfiguredTTL = time.Hour })
	_ = score(c2)
	c2.ResetCapabilities()
	_ = score(c2)
	if srv.count(cloudproto.PathUsage) != 3 {
		t.Fatalf("usage=%d", srv.count(cloudproto.PathUsage))
	}
}

// ---- m6: concurrent first calls share one probe ----

func TestCloud_ConcurrentFirstCallsShareOneProbe(t *testing.T) {
	const callers = 8
	var scoreCalls, usageCalls atomic.Int32
	release := make(chan struct{})
	c := New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, cloudproto.PathScore) {
				if scoreCalls.Add(1) == callers {
					close(release) // every caller has met the 404; now let the probe answer
				}
				return jsonResp(404, "404 page not found"), nil
			}
			usageCalls.Add(1)
			<-release
			return jsonResp(200, `{"product":"sneat"}`), nil
		})}})
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = score(c) }()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, decision.ErrUnsupported) {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if usageCalls.Load() != 1 {
		t.Fatalf("%d probes for %d concurrent callers", usageCalls.Load(), callers)
	}
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: readCloser(body)}
}

func readCloser(s string) *nopCloser { return &nopCloser{strings.NewReader(s)} }

type nopCloser struct{ *strings.Reader }

func (*nopCloser) Close() error { return nil }

// A follower whose leader was cancelled by the LEADER's context probes itself,
// and a follower that is cancelled itself stops waiting.
func TestCloud_FollowersSurviveACancelledLeaderAndStopWaitingWhenCancelled(t *testing.T) {
	leaderIn := make(chan struct{})
	var usageCalls atomic.Int32
	c := New(Config{BaseURL: "https://unused.example/v0/", Product: "sneat", Token: tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, cloudproto.PathScore) {
				return jsonResp(404, "404 page not found"), nil
			}
			if usageCalls.Add(1) == 1 { // the leader's probe: hangs until its context is cancelled
				close(leaderIn)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return jsonResp(200, `{"product":"sneat"}`), nil
		})}})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := c.Decider().(decision.ScoredProvider).Score(leaderCtx, libraryScoreRequest())
		leaderDone <- err
	}()
	<-leaderIn

	// A follower that gives up on its own.
	impatient, giveUp := context.WithCancel(context.Background())
	impatientDone := make(chan error, 1)
	go func() {
		_, err := c.Decider().(decision.ScoredProvider).Score(impatient, libraryScoreRequest())
		impatientDone <- err
	}()
	// A follower that keeps waiting.
	patientDone := make(chan error, 1)
	go func() { patientDone <- score(c) }()

	// Both followers must be waiting on the shared probe before the leader goes away.
	waitUntil(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.probe != nil
	})
	time.Sleep(50 * time.Millisecond)
	giveUp()
	var aiErr *ai.Error
	if err := <-impatientDone; !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("impatient follower: %v", err)
	}
	cancelLeader()
	if err := <-leaderDone; !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("leader: %v", err)
	}
	if err := <-patientDone; !errors.Is(err, decision.ErrUnsupported) {
		t.Fatalf("a follower must not inherit the leader's cancellation: %v", err)
	}
	if usageCalls.Load() != 2 {
		t.Fatalf("usage calls = %d", usageCalls.Load())
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// ---- M1: what a server can claim about a decision ----

func decisionServer(t *testing.T, body string) *Client {
	t.Helper()
	return newProtoServer(t, map[string]route{cloudproto.PathDecision: jsonRoute(200, body)}).client()
}

func remote(t *testing.T, decisionJSON string) decision.Decision {
	t.Helper()
	d, ok, err := decisionServer(t, `{"decided":true,"decision":`+decisionJSON+`}`).Decider().Decide(context.Background(), decideReq())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	return d
}

const remoteBase = `"module":{"value":"m","confidence":1},"intent":{"value":"i","confidence":1},"interaction":"command"`

func TestCloud_AServerCannotMakeADecisionDeterministicOrPreJudgeItActionable(t *testing.T) {
	for _, claim := range []string{
		`"outcome":"deterministic","provenance":"deterministic","deterministic":true`,
		`"outcome":"accepted"`, `"outcome":"selected"`, `"outcome":"several"`, `"outcome":"floor"`,
	} {
		d := remote(t, `{`+remoteBase+`,`+claim+`}`)
		if d.Provenance() != decision.ProvenanceSelfReported || d.Outcome != "" || d.Actionable() {
			t.Fatalf("%s: %+v", claim, d)
		}
		// Not even under a durable chain that also accepts a self-reported 1.0.
		pol := decision.DurablePolicy()
		pol.AcceptUncalibratedAt = 1
		c := decisionServer(t, `{"decided":true,"decision":{`+remoteBase+`,`+claim+`}}`)
		got, ok, tr := decision.Chain{Providers: []decision.Provider{c.Decider()}, Policy: &pol}.Decide(context.Background(), decideReq())
		if !ok || got.Outcome != decision.OutcomeAccepted || tr.Provenance != decision.ProvenanceSelfReported {
			// accepted by the explicit bar, as a self-reported answer, never deterministic
			t.Fatalf("%s: ok=%v d=%+v tr=%+v", claim, ok, got, tr)
		}
		durable := decision.DurablePolicy()
		if got, ok, _ := (decision.Chain{Providers: []decision.Provider{c.Decider()}, Policy: &durable}).Decide(context.Background(), decideReq()); ok || got.Actionable() {
			t.Fatalf("%s: durable acted on a server's claim: %+v", claim, got)
		}
	}
	// A refusal from the server is advisory input and is not kept: the local policy
	// judges (see TestCloud_AServerRefusalIsAdvisoryAndTheLocalPolicyJudges), and a
	// calibrated answer keeps its flag.
	for _, o := range []string{"uncertain", "none", "unscored", "invalid"} {
		if d := remote(t, `{`+remoteBase+`,"outcome":"`+o+`"}`); d.Outcome != "" || d.Actionable() {
			t.Fatalf("%s: %+v", o, d)
		}
	}
	if d := remote(t, `{`+remoteBase+`,"calibrated":true,"scores":{"m/i":0.97,"m/j":0.03}}`); d.Provenance() != decision.ProvenanceCalibrated || d.Outcome != "" {
		t.Fatalf("%+v", d)
	}
}

// m3: one rule. A server's "uncertain" over calibrated scores is advisory: under a
// chain policy the local verdict stands (selected here), and the trace shows the
// local verdict only.
func TestCloud_AServerRefusalIsAdvisoryAndTheLocalPolicyJudges(t *testing.T) {
	body := `{"decided":true,"decision":{` + remoteBase + `,"calibrated":true,"scores":{"m/i":0.97,"m/j":0.03},"outcome":"uncertain"}}`
	c := decisionServer(t, body)
	pol := decision.NarrowingPolicy()
	got, ok, tr := decision.Chain{Providers: []decision.Provider{c.Decider()}, Policy: &pol}.Decide(context.Background(), decideReq())
	if !ok || got.Outcome != decision.OutcomeSelected || !got.Actionable() || tr.Outcome != decision.OutcomeSelected {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
}

// A call that met the same 404 as a probe that has since finished is answered by
// that probe's verdict, never by a second probe.
func TestCloud_ALateCallIsAnsweredByTheRecordedVerdict(t *testing.T) {
	srv := newProtoServer(t, scoreRoutes(plainMissing, usageOK))
	c := srv.client()
	c.rememberAbsent()
	if err := c.routeVerdict(context.Background(), 404); !errors.Is(err, decision.ErrUnsupported) || srv.count(cloudproto.PathUsage) != 0 {
		t.Fatalf("err=%v usage=%d", err, srv.count(cloudproto.PathUsage))
	}
}

// S1, the review probe: a server answers a module-less undo with calibrated:true,
// no scores and no confidence. A chain, with or without a policy, must not hand
// that to a client that would execute the undo; an interaction confidence with the
// interaction's own scores is what a calibrated side-effectful claim needs.
func TestCloud_ACalibratedClaimAloneNeverMakesASideEffectfulInteractionActionable(t *testing.T) {
	dur, nar := decision.DurablePolicy(), decision.NarrowingPolicy()
	chains := map[string]decision.Chain{"no policy": {}, "durable": {Policy: &dur}, "narrowing": {Policy: &nar}, "accept any": {MinConfidence: -1}}
	for _, in := range []string{"undo", "confirmation", "rejection", "correction", "cancellation"} {
		c := decisionServer(t, `{"decided":true,"decision":{"module":{"value":"","confidence":0},"intent":{"value":"","confidence":0},"interaction":"`+in+`","calibrated":true}}`)
		for name, chain := range chains {
			chain.Providers = []decision.Provider{c.Decider()}
			if d, ok, tr := chain.Decide(context.Background(), decideReq()); ok || d.Actionable() {
				t.Fatalf("%s %s: ok=%v %+v %+v", name, in, ok, d, tr)
			}
		}
	}
	// With the interaction's scores and a positive confidence the claim is backed.
	body := `{"decided":true,"decision":{` + remoteBase + `,"interaction":"confirmation","interactionConfidence":0.96,` +
		`"interactionScores":{"confirmation":0.96,"command":0.04},"calibrated":true,"scores":{"m/i":0.97,"m/j":0.03}}}`
	body = strings.Replace(body, `,"interaction":"command"`, "", 1)
	c := decisionServer(t, body)
	got, ok, _ := decision.Chain{Providers: []decision.Provider{c.Decider()}, Policy: &nar}.Decide(context.Background(), decideReq())
	if !ok || !got.Actionable() || got.Outcome != decision.OutcomeSelected {
		t.Fatalf("ok=%v %+v", ok, got)
	}
}
