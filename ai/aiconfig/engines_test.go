package aiconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
	"github.com/strongo/aichat/ai/decision/rules"
	"github.com/strongo/aichat/ai/session"
)

// stubEngine is a scripted decision engine that counts its calls.
type stubEngine struct {
	name  string
	d     decision.Decision
	ok    bool
	err   error
	calls int
}

func (s *stubEngine) Name() string { return s.name }
func (s *stubEngine) Decide(context.Context, decision.Request) (decision.Decision, bool, error) {
	s.calls++
	return s.d, s.ok, s.err
}

func engineDecision(intent string) decision.Decision {
	return decision.Decision{
		Module: decision.Scored{Value: "m", Confidence: 0.9}, Intent: decision.Scored{Value: intent, Confidence: 0.9},
		Interaction: decision.InteractionCommand,
	}
}

func engineRequest() decision.Request {
	return decision.Request{Taxonomy: decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "m", Intents: []string{"i", "j"}}}}}
}

func decideVia(t *testing.T, p decision.Provider) (decision.Decision, bool, decision.Report, error) {
	t.Helper()
	tp, ok := p.(decision.TracedProvider)
	if !ok {
		t.Fatalf("%T is not a TracedProvider", p)
	}
	return tp.DecideTraced(context.Background(), engineRequest())
}

func buildDecision(t *testing.T, mutate func(*Config), deps Deps) (Providers, error) {
	t.Helper()
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "anthropic"}
	mutate(&cfg)
	if deps.Getenv == nil {
		deps.Getenv = func(string) string { return "" }
	}
	return Build(cfg, deps)
}

func TestLoad_ParsesEngineConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.yaml")
	yaml := `
decision:
  provider: auto
  engines: [jev, llm-decider]
  strategy: hedged
  hedgeAfter: 600ms
  breaker: false
  fallbackOn: [abstain]
  policy: narrowing
`
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Decision
	if !reflect.DeepEqual(d.Engines, []string{"jev", "llm-decider"}) || d.Strategy != "hedged" || d.HedgeAfter != "600ms" ||
		d.Breaker == nil || *d.Breaker || !reflect.DeepEqual(d.FallbackOn, []string{"abstain"}) || d.Policy != "narrowing" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestApplyEnv_EngineSettings(t *testing.T) {
	cfg := defaults()
	env := map[string]string{
		"X_" + EnvDecisionEngines:    " jev , llm-decider ,, ",
		"X_" + EnvDecisionStrategy:   "race",
		"X_" + EnvDecisionHedgeAfter: "1s",
		"X_" + EnvDecisionPolicy:     "durable",
	}
	cfg.ApplyEnv(func(k string) string { return env[k] }, "X_")
	d := cfg.Decision
	if !reflect.DeepEqual(d.Engines, []string{"jev", "llm-decider"}) || d.Strategy != "race" || d.HedgeAfter != "1s" || d.Policy != "durable" {
		t.Fatalf("decision = %+v", d)
	}
	if s := cfg.String(); !strings.Contains(s, "Decision.Engines:[jev llm-decider]") || !strings.Contains(s, "Decision.Strategy:race") {
		t.Fatalf("String() = %s", s)
	}
}

func TestBuild_NoEnginesConfiguredAddsNone(t *testing.T) {
	p, err := buildDecision(t, func(*Config) {}, Deps{Engines: map[string]decision.Provider{"jev": &stubEngine{name: "jev"}}})
	if err != nil || len(p.Decision) != 0 || p.Policy != nil {
		t.Fatalf("decision=%v policy=%v err=%v", p.Decision, p.Policy, err)
	}
}

func TestBuild_SingleEngineIsTheDefaultForOne(t *testing.T) {
	jev := &stubEngine{name: "jev", d: engineDecision("i"), ok: true}
	p, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev"} }, Deps{Engines: map[string]decision.Provider{"jev": jev}})
	if err != nil || len(p.Decision) != 1 {
		t.Fatalf("decision=%v err=%v", p.Decision, err)
	}
	d, ok, rep, err := decideVia(t, p.Decision[0])
	if err != nil || !ok || d.Intent.Value != "i" || rep.Strategy != "single" || rep.Engine != "jev" {
		t.Fatalf("d=%+v ok=%v rep=%+v err=%v", d, ok, rep, err)
	}
}

func TestBuild_TwoEnginesDefaultToFallbackAndFailOver(t *testing.T) {
	jev := &stubEngine{name: "jev", err: errors.New("down")}
	llm := &stubEngine{name: "llm-decider", d: engineDecision("j"), ok: true}
	p, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "llm-decider"} },
		Deps{Engines: map[string]decision.Provider{"jev": jev, "llm-decider": llm}})
	if err != nil {
		t.Fatal(err)
	}
	d, ok, rep, err := decideVia(t, p.Decision[0])
	if err != nil || !ok || d.Intent.Value != "j" || rep.Strategy != "fallback" || !rep.FallbackFired {
		t.Fatalf("d=%+v ok=%v rep=%+v err=%v", d, ok, rep, err)
	}
}

func TestBuild_HedgedRaceAndStrategyShapes(t *testing.T) {
	engines := map[string]decision.Provider{
		"a": &stubEngine{name: "a", d: engineDecision("i"), ok: true},
		"b": &stubEngine{name: "b", d: engineDecision("j"), ok: true},
		"c": &stubEngine{name: "c", d: engineDecision("j"), ok: true},
	}
	cases := []struct {
		name     string
		engines  []string
		strategy string
		hedge    string
		wantErr  string
		want     compose.Strategy
	}{
		{"hedged default budget", []string{"a", "b"}, "hedged", "", "", compose.StrategyHedged},
		{"hedged explicit budget", []string{"a", "b"}, "hedged", "250ms", "", compose.StrategyHedged},
		{"hedged bad duration", []string{"a", "b"}, "hedged", "soon", "not a non-negative duration", ""},
		{"hedged negative duration", []string{"a", "b"}, "hedged", "-1s", "not a non-negative duration", ""},
		{"hedged needs two", []string{"a", "b", "c"}, "hedged", "", "needs exactly 2", ""},
		{"race of three", []string{"a", "b", "c"}, "race", "", "", compose.StrategyRace},
		{"race needs two", []string{"a"}, "race", "", "needs at least 2", ""},
		{"fallback needs two", []string{"a"}, "fallback", "", "needs exactly 2", ""},
		{"single needs one", []string{"a", "b"}, "single", "", "needs exactly 1", ""},
		{"three need an explicit strategy", []string{"a", "b", "c"}, "", "", "needs exactly 1", ""},
		{"unknown strategy", []string{"a", "b"}, "democracy", "", "unknown decision.strategy", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildDecision(t, func(c *Config) {
				c.Decision.Engines, c.Decision.Strategy, c.Decision.HedgeAfter = tc.engines, tc.strategy, tc.hedge
			}, Deps{Engines: engines})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := p.Decision[0].(*compose.Engine).Strategy(); got != tc.want {
				t.Fatalf("strategy = %q want %q", got, tc.want)
			}
		})
	}
}

func TestBuild_UnknownEngineName(t *testing.T) {
	_, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"ghost"} }, Deps{})
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuild_EnginesRunAfterExtrasAndBeforeCloud(t *testing.T) {
	jev := &stubEngine{name: "jev", d: engineDecision("i"), ok: true}
	p, err := Build(Config{
		LLM:      LLM{Provider: "cloud"},
		Decision: Decision{Provider: "auto", Engines: []string{"jev"}},
	}, Deps{
		CloudToken: tokenFunc("t"), CloudBaseURL: "https://api.example.com/",
		ExtraDecision: []decision.Provider{fakeDecider{name: "product-rules"}},
		Engines:       map[string]decision.Provider{"jev": jev},
		Getenv:        func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range p.Decision {
		names = append(names, d.Name())
	}
	if want := []string{"product-rules", "single(jev)", "cloud-decision"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("chain = %v want %v", names, want)
	}
}

func TestBuild_DisabledDecisionSkipsEngines(t *testing.T) {
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Provider, c.Decision.Engines = "disabled", []string{"jev"}
	}, Deps{Engines: map[string]decision.Provider{"jev": &stubEngine{name: "jev"}}})
	if err != nil || len(p.Decision) != 0 {
		t.Fatalf("decision=%v err=%v", p.Decision, err)
	}
}

func TestBuild_BreakerDefaultsOnAndCanBeDisabled(t *testing.T) {
	run := func(breaker *bool) (calls int, transitions []string) {
		down := &stubEngine{name: "jev", err: errors.New("down")}
		p, err := buildDecision(t, func(c *Config) {
			c.Decision.Engines, c.Decision.Breaker = []string{"jev"}, breaker
		}, Deps{
			Engines: map[string]decision.Provider{"jev": down},
			Clock:   compose.SystemClock(),
			OnBreakerChange: func(engine string, from, to compose.BreakerState) {
				transitions = append(transitions, engine+":"+from.String()+">"+to.String())
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			_, _, _ = p.Decision[0].Decide(context.Background(), engineRequest())
		}
		return down.calls, transitions
	}
	calls, moves := run(nil)
	if calls != compose.DefaultBreakerThreshold || !reflect.DeepEqual(moves, []string{"jev:closed>open"}) {
		t.Fatalf("default breaker: calls=%d moves=%v", calls, moves)
	}
	off := false
	calls, moves = run(&off)
	if calls != 10 || len(moves) != 0 {
		t.Fatalf("breaker disabled: calls=%d moves=%v", calls, moves)
	}
}

func TestBuild_FallbackOnOptIns(t *testing.T) {
	jev := &stubEngine{name: "jev"} // abstains
	llm := &stubEngine{name: "llm-decider", d: engineDecision("j"), ok: true}
	engines := map[string]decision.Provider{"jev": jev, "llm-decider": llm}
	build := func(on ...string) decision.Provider {
		p, err := buildDecision(t, func(c *Config) {
			c.Decision.Engines, c.Decision.FallbackOn = []string{"jev", "llm-decider"}, on
		}, Deps{Engines: engines})
		if err != nil {
			t.Fatal(err)
		}
		return p.Decision[0]
	}
	if _, ok, _, _ := decideVia(t, build()); ok || llm.calls != 0 {
		t.Fatalf("an abstention must not start the backup by default (llm calls=%d)", llm.calls)
	}
	if _, ok, _, _ := decideVia(t, build("abstain", "uncertain")); !ok || llm.calls != 1 {
		t.Fatalf("fallbackOn abstain should start the backup (llm calls=%d)", llm.calls)
	}
	_, err := buildDecision(t, func(c *Config) {
		c.Decision.Engines, c.Decision.FallbackOn = []string{"jev"}, []string{"sometimes"}
	}, Deps{Engines: engines})
	if err == nil || !strings.Contains(err.Error(), "fallbackOn") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuild_PolicyByName(t *testing.T) {
	for name, want := range map[string]decision.SelectionPolicy{
		"narrowing": decision.NarrowingPolicy(), "durable": decision.DurablePolicy(),
	} {
		p, err := buildDecision(t, func(c *Config) { c.Decision.Policy = name }, Deps{})
		if err != nil || p.Policy == nil || *p.Policy != want {
			t.Fatalf("%s: policy=%v err=%v", name, p.Policy, err)
		}
	}
	if _, err := buildDecision(t, func(c *Config) { c.Decision.Policy = "yolo" }, Deps{}); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestBuild_PolicyReachesEnginesUncertainOptIn(t *testing.T) {
	uncertain := engineDecision("i")
	uncertain.Calibrated, uncertain.Scores = true, map[string]float64{"m/i": 0.38, "m/j": 0.35, "x": 0.33}
	uncertain.Intent.Confidence, uncertain.Module.Confidence = 0.07, 0.07
	jev := &stubEngine{name: "jev", d: uncertain, ok: true}
	llm := &stubEngine{name: "llm-decider", d: engineDecision("j"), ok: true}
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Engines = []string{"jev", "llm-decider"}
		c.Decision.FallbackOn, c.Decision.Policy = []string{"uncertain"}, "narrowing"
	}, Deps{Engines: map[string]decision.Provider{"jev": jev, "llm-decider": llm}})
	if err != nil {
		t.Fatal(err)
	}
	d, ok, rep, _ := decideVia(t, p.Decision[0])
	if !ok || d.Intent.Value != "j" || !rep.FallbackFired {
		t.Fatalf("d=%+v ok=%v rep=%+v", d, ok, rep)
	}
	if p.Policy == nil || p.Policy.Name != "narrowing" {
		t.Fatalf("policy = %v", p.Policy)
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" a, b ,,c "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("%v", got)
	}
}

func f(v float64) *float64 { return &v }

func TestBuild_PolicyValuesOverrideTheNamedPolicyAndAreValidated(t *testing.T) {
	maxPicks := 3
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Policy = "narrowing"
		c.Decision.PolicyValues = &PolicyValues{
			MinConfidence: f(0.6), MinGap: f(0.1), MinProbability: f(0.7), StrongProbability: f(0.9), PotentialProbability: f(0.4), MaxPicks: &maxPicks,
		}
	}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	want := decision.SelectionPolicy{Name: "narrowing+custom", MinConfidence: 0.6, MinGap: 0.1, MinProbability: 0.7, StrongProbability: 0.9, PotentialProbability: 0.4, MaxPicks: 3, AcceptUncalibratedAt: decision.NarrowingAcceptUncalibratedAt}
	if p.Policy == nil || *p.Policy != want {
		t.Fatalf("policy = %+v", p.Policy)
	}

	// Values left unset keep the named policy's.
	p, err = buildDecision(t, func(c *Config) {
		c.Decision.Policy, c.Decision.PolicyValues = "durable", &PolicyValues{MinProbability: f(0.92)}
	}, Deps{})
	base := decision.DurablePolicy()
	base.Name, base.MinProbability = "durable+custom", 0.92
	if err != nil || *p.Policy != base {
		t.Fatalf("partial override: %+v %v", p.Policy, err)
	}

	for name, mutate := range map[string]func(*Config){
		"overrides without a policy": func(c *Config) { c.Decision.PolicyValues = &PolicyValues{MinGap: f(0.1)} },
		"out of range": func(c *Config) {
			c.Decision.Policy, c.Decision.PolicyValues = "narrowing", &PolicyValues{MinGap: f(1.5)}
		},
		"zero threshold": func(c *Config) {
			c.Decision.Policy, c.Decision.PolicyValues = "narrowing", &PolicyValues{MinProbability: f(0), PotentialProbability: f(0)}
		},
		"unordered": func(c *Config) {
			c.Decision.Policy, c.Decision.PolicyValues = "narrowing", &PolicyValues{StrongProbability: f(0.5)}
		},
	} {
		if _, err := buildDecision(t, mutate, Deps{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoad_ParsesPolicyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai.yaml")
	yaml := "decision:\n  policy: narrowing\n  policyValues:\n    minProbability: 0.7\n    maxPicks: 4\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	v := cfg.Decision.PolicyValues
	if v == nil || v.MinProbability == nil || *v.MinProbability != 0.7 || v.MaxPicks == nil || *v.MaxPicks != 4 || v.MinGap != nil {
		t.Fatalf("values = %+v", v)
	}
}

// fallbackOn quota is the opt-in to a paid backup taking over an exhausted allowance.
func TestBuild_FallbackOnQuotaIsAnOptIn(t *testing.T) {
	jev := &stubEngine{name: "jev", err: fmt.Errorf("allowance: %w", decision.ErrQuota)}
	llm := &stubEngine{name: "llm-decider", d: engineDecision("j"), ok: true}
	engines := map[string]decision.Provider{"jev": jev, "llm-decider": llm}
	build := func(on ...string) decision.Provider {
		p, err := buildDecision(t, func(c *Config) {
			c.Decision.Engines, c.Decision.FallbackOn = []string{"jev", "llm-decider"}, on
		}, Deps{Engines: engines})
		if err != nil {
			t.Fatal(err)
		}
		return p.Decision[0]
	}
	if _, ok, _, err := decideVia(t, build()); ok || !errors.Is(err, decision.ErrQuota) || llm.calls != 0 {
		t.Fatalf("by default the quota error is surfaced and the paid backup untouched: ok=%v err=%v calls=%d", ok, err, llm.calls)
	}
	if _, ok, _, err := decideVia(t, build("quota")); !ok || err != nil || llm.calls != 1 {
		t.Fatalf("fallbackOn quota: ok=%v err=%v calls=%d", ok, err, llm.calls)
	}
}

func TestBuild_PolicyValuesCanSetTheUncalibratedBar(t *testing.T) {
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Policy, c.Decision.PolicyValues = "durable", &PolicyValues{AcceptUncalibratedAt: f(0.85)}
	}, Deps{})
	if err != nil || p.Policy.AcceptUncalibratedAt != 0.85 || p.Policy.Name != "durable+custom" {
		t.Fatalf("policy=%+v err=%v", p.Policy, err)
	}
	if _, err := buildDecision(t, func(c *Config) {
		c.Decision.Policy, c.Decision.PolicyValues = "durable", &PolicyValues{AcceptUncalibratedAt: f(1.5)}
	}, Deps{}); err == nil || !strings.Contains(err.Error(), "acceptUncalibratedAt") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_ParsesBreakerSlowThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai.yaml")
	if err := os.WriteFile(path, []byte("decision:\n  breakerSlowThreshold: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Decision.BreakerSlowThreshold != 4 {
		t.Fatalf("cfg=%+v err=%v", cfg.Decision, err)
	}
}

// ---- M1: the aiconfig layout (the product's rules first, then the engines) ----

func TestBuild_DurablePolicyKeepsRuleMatchesDeterministicAndFree(t *testing.T) {
	rule := rules.New("rules", rules.Rule{Name: "show", Match: func(text string, _ session.State) (decision.Decision, bool) {
		return decision.Decision{Module: decision.Scored{Value: "m"}, Intent: decision.Scored{Value: "i"}, Interaction: decision.InteractionCommand}, text == "show"
	}})
	jev := &stubEngine{name: "jev", d: engineDecision("j"), ok: true}
	p, err := buildDecision(t, func(c *Config) { c.Decision.Engines, c.Decision.Policy = []string{"jev"}, "durable" },
		Deps{ExtraDecision: []decision.Provider{rule}, Engines: map[string]decision.Provider{"jev": jev}})
	if err != nil {
		t.Fatal(err)
	}
	req := engineRequest()
	req.Text = "Show!"
	d, ok, tr := p.Chain().Decide(context.Background(), req)
	if !ok || d.Outcome != decision.OutcomeDeterministic || !d.Actionable() || jev.calls != 0 || tr.DecidedBy != "rules" || tr.Provenance != decision.ProvenanceDeterministic {
		t.Fatalf("a rule match under policy: durable must be accepted without a paid call: ok=%v d=%+v jev=%d tr=%+v", ok, d, jev.calls, tr)
	}
	// No rule matches: the engine decides, and its uncalibrated answer is not accepted by durable.
	req.Text = "something else"
	if d, ok, _ := p.Chain().Decide(context.Background(), req); ok || d.Actionable() || jev.calls != 1 {
		t.Fatalf("ok=%v d=%+v calls=%d", ok, d, jev.calls)
	}
}

// ---- m5: stop on quota / misconfiguration ----

func TestBuild_ChainStopsAtQuotaAndMisconfigurationUnlessOptedOut(t *testing.T) {
	quota := fmt.Errorf("jev: %w", decision.ErrQuota)
	mis := fmt.Errorf("jev: %w", decision.ErrMisconfigured)
	for name, tc := range map[string]struct {
		err      error
		mutate   func(*Config)
		stopped  string
		cloudHit int
	}{
		"quota, default":                   {quota, func(*Config) {}, decision.AttemptQuota, 0},
		"quota, explicit on":               {quota, func(c *Config) { c.Decision.StopOnQuota = ptr(true) }, decision.AttemptQuota, 0},
		"quota, opted out":                 {quota, func(c *Config) { c.Decision.StopOnQuota = ptr(false) }, "", 1},
		"quota, only misconfigured is off": {quota, func(c *Config) { c.Decision.StopOnMisconfigured = ptr(false) }, decision.AttemptQuota, 0},
		"misconfigured, default":           {mis, func(*Config) {}, decision.AttemptMisconfigured, 0},
		"misconfigured, opted out":         {mis, func(c *Config) { c.Decision.StopOnMisconfigured = ptr(false) }, "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			var hits int
			deps := Deps{
				Product: "p", CloudBaseURL: "https://cloud.example/v0/", CloudToken: func(context.Context) (string, error) { return "t", nil },
				HTTPClient: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
					hits++
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"decided":false}`))}, nil
				})},
				Engines: map[string]decision.Provider{"jev": &stubEngine{name: "jev", err: tc.err}},
			}
			p, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev"}; tc.mutate(c) }, deps)
			if err != nil {
				t.Fatal(err)
			}
			_, ok, tr := p.Chain().Decide(context.Background(), engineRequest())
			if ok || tr.StoppedBy != tc.stopped || hits != tc.cloudHit {
				t.Fatalf("ok=%v stoppedBy=%q cloud calls=%d tr=%+v", ok, tr.StoppedBy, hits, tr)
			}
			if tc.stopped != "" && tr.Err() == nil {
				t.Fatal("a stopped chain has an error")
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ptr[T any](v T) *T { return &v }

func TestProviders_ChainCarriesProvidersPolicyAndStopSettings(t *testing.T) {
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Policy = "narrowing"
		c.Decision.StopOnQuota = ptr(false)
	}, Deps{ExtraDecision: []decision.Provider{&stubEngine{name: "rules"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Chain()
	if len(c.Providers) != 1 || c.Policy == nil || c.Policy.Name != "narrowing" || c.StopOnQuota != decision.StopOff || c.StopOnMisconfigured != decision.StopOn {
		t.Fatalf("%+v", c)
	}
}

func TestLoad_ParsesStopOptions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.yaml")
	if err := os.WriteFile(p, []byte("decision:\n  stopOnQuota: false\n  stopOnMisconfigured: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.Decision.StopOnQuota == nil || *cfg.Decision.StopOnQuota || cfg.Decision.StopOnMisconfigured == nil || !*cfg.Decision.StopOnMisconfigured {
		t.Fatalf("%+v %v", cfg.Decision, err)
	}
}

func TestBuild_PolicyValuesSetTheSideEffectOptIn(t *testing.T) {
	p, err := buildDecision(t, func(c *Config) {
		c.Decision.Policy, c.Decision.PolicyValues = "narrowing", &PolicyValues{AcceptUncalibratedSideEffects: ptr(true)}
	}, Deps{})
	if err != nil || p.Policy == nil || !p.Policy.AcceptUncalibratedSideEffects {
		t.Fatalf("%+v %v", p.Policy, err)
	}
	p, err = buildDecision(t, func(c *Config) { c.Decision.Policy = "narrowing" }, Deps{})
	if err != nil || p.Policy.AcceptUncalibratedSideEffects {
		t.Fatalf("the opt-in is off by default: %+v %v", p.Policy, err)
	}
}
