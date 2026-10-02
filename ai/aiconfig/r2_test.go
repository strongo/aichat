package aiconfig

import (
	"context"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
	"github.com/strongo/aichat/ai/decision/rules"
)

// R2 major through configuration: the primary answers uncertain (or abstains) and
// the backup is budgeted; after the cap the chain must stop, not run the next
// provider on every turn.
func TestBuild_ASpentBackupBudgetStopsTheChainForUncertainAndAbstainingPrimaries(t *testing.T) {
	const turns, cap = 50, 3
	uncertain := engineDecision("i")
	uncertain.Calibrated, uncertain.Scores = true, map[string]float64{"m/i": 0.55, "m/j": 0.45}
	uncertain.Module.Confidence, uncertain.Intent.Confidence = 0.6, 0.6
	for name, tc := range map[string]struct {
		strategy   string
		primary    *stubEngine
		fallbackOn []string
		policy     string
	}{
		"fallback, uncertain":     {"fallback", &stubEngine{name: "jev", d: uncertain, ok: true}, []string{"uncertain"}, "narrowing"},
		"hedged, uncertain":       {"hedged", &stubEngine{name: "jev", d: uncertain, ok: true}, []string{"uncertain"}, "narrowing"},
		"fallback, abstain+quota": {"fallback", &stubEngine{name: "jev"}, []string{"abstain", "quota"}, ""},
		"hedged, abstain+quota":   {"hedged", &stubEngine{name: "jev"}, []string{"abstain", "quota"}, ""},
		"fallback, abstain":       {"fallback", &stubEngine{name: "jev"}, []string{"abstain"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			llm := &stubEngine{name: "llm", d: engineDecision("i"), ok: true}
			deps := Deps{Engines: map[string]decision.Provider{"jev": tc.primary, "llm": llm}}
			p, err := buildDecision(t, func(c *Config) {
				c.Decision.Engines, c.Decision.Strategy, c.Decision.FallbackOn, c.Decision.Policy = []string{"jev", "llm"}, tc.strategy, tc.fallbackOn, tc.policy
				c.Decision.BackupBudget = &BackupBudget{MaxCalls: cap}
			}, deps)
			if err != nil {
				t.Fatal(err)
			}
			next := &stubEngine{name: "cloud-decider"}
			chain := p.Chain()
			chain.Providers = append(chain.Providers, next)
			var decided, stopped int
			var last decision.Trace
			for i := 0; i < turns; i++ {
				_, ok, tr := chain.Decide(context.Background(), engineRequest())
				last = tr
				if ok {
					decided++
				} else if tr.StoppedBy != "" {
					stopped++
				}
			}
			if llm.calls != cap || decided != cap || stopped != turns-cap || next.calls != 0 || last.StoppedBy != decision.AttemptBudget {
				t.Fatalf("backup calls=%d decided=%d stopped=%d next-provider calls=%d last=%+v", llm.calls, decided, stopped, next.calls, last)
			}
		})
	}
}

// m-f: rules wrapped in an engine, a breaker or a budget are still rules.
func TestBuild_WrappedRulesAfterAnotherEngineAreRejected(t *testing.T) {
	mk := func() *rules.Provider { return rules.New("rules") }
	for name, wrap := range map[string]func(decision.Provider) decision.Provider{
		"single":  func(p decision.Provider) decision.Provider { return compose.Single(p) },
		"breaker": func(p decision.Provider) decision.Provider { return compose.NewBreaker(p) },
		"budget": func(p decision.Provider) decision.Provider {
			return compose.NewBudget(p, compose.BudgetOptions{MaxCalls: 1})
		},
		"nested": func(p decision.Provider) decision.Provider { return compose.NewBreaker(compose.Single(p)) },
	} {
		deps, _, _ := budgetDeps()
		deps.Engines["wrapped"] = wrap(mk())
		if _, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "wrapped"} }, deps); err == nil || !strings.Contains(err.Error(), "Deps.ExtraDecision") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A wrapped non-deterministic engine is fine.
	deps, _, _ := budgetDeps()
	deps.Engines["wrapped"] = compose.Single(deps.Engines["llm"])
	if _, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "wrapped"} }, deps); err != nil {
		t.Fatal(err)
	}
}

// m-g: Build reports what an earlier ApplyEnv could not read, once.
func TestBuild_KeepsErrorsFromACallersEarlierApplyEnv(t *testing.T) {
	bad := func(k string) string {
		if k == EnvDecisionStopOnQuota {
			return "perhaps"
		}
		return ""
	}
	cfg := defaults()
	cfg.LLM.Provider = "byok"
	cfg.BYOK = BYOK{Protocol: "anthropic"}
	cfg.ApplyEnv(bad, "")
	// Build sees a clean environment: the earlier error must still be reported.
	_, err := Build(cfg, Deps{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), EnvDecisionStopOnQuota) {
		t.Fatalf("an earlier ApplyEnv error was dropped: %v", err)
	}
	// The same bad value applied twice (by the caller and by Build) is one message.
	_, err = Build(cfg, Deps{Getenv: bad})
	if err == nil || strings.Count(err.Error(), "is not a boolean") != 1 {
		t.Fatalf("%v", err)
	}
}

func TestEnvBooleanDocMatchesParseBool(t *testing.T) {
	// The documented forms are the ones strconv.ParseBool reads; a padded value is an error.
	for v, want := range map[string]bool{"1": true, "t": true, "TRUE": true, "True": true, "0": false, "F": false, "false": false} {
		cfg := defaults()
		cfg.ApplyEnv(func(k string) string {
			if k == EnvDecisionStopOnQuota {
				return v
			}
			return ""
		}, "")
		if len(cfg.envErrs) != 0 || cfg.Decision.StopOnQuota == nil || *cfg.Decision.StopOnQuota != want {
			t.Errorf("%q: %v %v", v, cfg.envErrs, cfg.Decision.StopOnQuota)
		}
	}
	cfg := defaults()
	cfg.ApplyEnv(func(k string) string {
		if k == EnvDecisionStopOnQuota {
			return " false"
		}
		return ""
	}, "")
	if len(cfg.envErrs) != 1 {
		t.Fatalf("%v", cfg.envErrs)
	}
}
