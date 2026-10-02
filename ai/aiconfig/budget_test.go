package aiconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/rules"
	"github.com/strongo/aichat/ai/session"
)

var errTransient = errors.New("jev: HTTP 429 (rate limited)")

func budgetDeps() (Deps, *stubEngine, *stubEngine) {
	jev := &stubEngine{name: "jev", err: errTransient}
	llm := &stubEngine{name: "llm", d: engineDecision("i"), ok: true}
	return Deps{Engines: map[string]decision.Provider{"jev": jev, "llm": llm}}, jev, llm
}

// S4: the primary fails transiently on every call; without a cap the paid backup
// takes all of it, with backupBudget it is bounded and the chain stops loudly.
func TestBuild_BackupBudgetBoundsThePaidBackup(t *testing.T) {
	const turns, cap = 40, 6
	for _, strategy := range []string{"fallback", "hedged"} {
		t.Run(strategy, func(t *testing.T) {
			run := func(budget *BackupBudget) (paid, decided, stopped int, last decision.Trace) {
				deps, _, llm := budgetDeps()
				p, err := buildDecision(t, func(c *Config) {
					c.Decision.Engines, c.Decision.Strategy, c.Decision.BackupBudget = []string{"jev", "llm"}, strategy, budget
				}, deps)
				if err != nil {
					t.Fatal(err)
				}
				chain := p.Chain()
				for i := 0; i < turns; i++ {
					_, ok, tr := chain.Decide(context.Background(), engineRequest())
					last = tr
					if ok {
						decided++
					} else if tr.StoppedBy != "" {
						stopped++
					}
				}
				return llm.calls, decided, stopped, last
			}
			if paid, decided, stopped, _ := run(nil); paid != turns || decided != turns || stopped != 0 {
				t.Fatalf("baseline (no budget): paid=%d decided=%d stopped=%d", paid, decided, stopped)
			}
			paid, decided, stopped, last := run(&BackupBudget{MaxCalls: cap, Per: "1h"})
			if paid != cap || decided != cap || stopped != turns-cap || last.StoppedBy != decision.AttemptBudget || !errors.Is(last.Err(), decision.ErrBudget) {
				t.Fatalf("paid=%d decided=%d stopped=%d last=%+v", paid, decided, stopped, last)
			}
		})
	}
}

func TestBuild_BackupBudgetFromEnvironment(t *testing.T) {
	deps, _, llm := budgetDeps()
	deps.Getenv = func(k string) string {
		return map[string]string{"AI_DECISION_BACKUP_BUDGET_MAX_CALLS": "2", "AI_DECISION_BACKUP_BUDGET_PER": "30m", "AI_DECISION_STOP_ON_QUOTA": "true", "AI_DECISION_STOP_ON_MISCONFIGURED": "false"}[k]
	}
	p, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "llm"} }, deps)
	if err != nil {
		t.Fatal(err)
	}
	if p.StopOnQuota != decision.StopChain || p.StopOnMisconfigured != decision.FallThrough {
		t.Fatalf("%+v", p)
	}
	for i := 0; i < 5; i++ {
		p.Chain().Decide(context.Background(), engineRequest())
	}
	if llm.calls != 2 {
		t.Fatalf("the environment budget of 2 was not applied: %d calls", llm.calls)
	}
	// Prefixed too.
	deps.Getenv = func(k string) string {
		return map[string]string{"X_AI_DECISION_STOP_ON_QUOTA": "false"}[k]
	}
	deps.EnvPrefix = "X_"
	if p, err = buildDecision(t, func(c *Config) {}, deps); err != nil || p.StopOnQuota != decision.FallThrough {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestBuild_MalformedStopAndBudgetEnvironmentIsAnError(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"stop on quota":         {"AI_DECISION_STOP_ON_QUOTA": "sometimes"},
		"stop on misconfigured": {"AI_DECISION_STOP_ON_MISCONFIGURED": "2x"},
		"max calls":             {"AI_DECISION_BACKUP_BUDGET_MAX_CALLS": "many"},
	} {
		deps, _, _ := budgetDeps()
		deps.Getenv = func(k string) string { return env[k] }
		_, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "llm"} }, deps)
		if err == nil || !strings.Contains(err.Error(), "aiconfig: AI_DECISION") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// ApplyEnv records every value it cannot parse for Build to report.
	cfg := defaults()
	cfg.ApplyEnv(func(string) string { return "nope" }, "")
	if len(cfg.envErrs) != 3 {
		t.Fatalf("%v", cfg.envErrs)
	}
}

func TestBuild_BackupBudgetMisconfigurationsAreLoud(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no engines": {func(c *Config) { c.Decision.BackupBudget = &BackupBudget{MaxCalls: 1} }, "needs decision.engines"},
		"single": {func(c *Config) {
			c.Decision.Engines, c.Decision.BackupBudget = []string{"jev"}, &BackupBudget{MaxCalls: 1}
		}, `not "single"`},
		"race": {func(c *Config) {
			c.Decision.Engines, c.Decision.Strategy, c.Decision.BackupBudget = []string{"jev", "llm"}, "race", &BackupBudget{MaxCalls: 1}
		}, `not "race"`},
		"zero cap": {func(c *Config) { c.Decision.Engines, c.Decision.BackupBudget = []string{"jev", "llm"}, &BackupBudget{} }, "maxCalls must be above 0"},
		"negative cap": {func(c *Config) {
			c.Decision.Engines, c.Decision.BackupBudget = []string{"jev", "llm"}, &BackupBudget{MaxCalls: -3}
		}, "got -3"},
		"bad per": {func(c *Config) {
			c.Decision.Engines, c.Decision.BackupBudget = []string{"jev", "llm"}, &BackupBudget{MaxCalls: 1, Per: "soon"}
		}, `"soon"`},
		"negative per": {func(c *Config) {
			c.Decision.Engines, c.Decision.BackupBudget = []string{"jev", "llm"}, &BackupBudget{MaxCalls: 1, Per: "-1h"}
		}, `"-1h"`},
		"quota both ways": {func(c *Config) {
			c.Decision.Engines, c.Decision.FallbackOn, c.Decision.StopOnQuota = []string{"jev", "llm"}, []string{"quota"}, ptr(true)
		}, "contradicts"},
	} {
		deps, _, _ := budgetDeps()
		if _, err := buildDecision(t, tc.mutate, deps); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An unset Per means a cap that never resets; and the quota pair is fine when
	// stopOnQuota is unset or false (the failover is then explicit).
	deps, _, _ := budgetDeps()
	if _, err := buildDecision(t, func(c *Config) {
		c.Decision.Engines, c.Decision.BackupBudget = []string{"jev", "llm"}, &BackupBudget{MaxCalls: 1}
	}, deps); err != nil {
		t.Fatal(err)
	}
	for name, stop := range map[string]*bool{"unset": nil, "false": ptr(false)} {
		deps, _, _ := budgetDeps()
		if _, err := buildDecision(t, func(c *Config) {
			c.Decision.Engines, c.Decision.FallbackOn, c.Decision.StopOnQuota = []string{"jev", "llm"}, []string{"quota"}, stop
		}, deps); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoad_ParsesBackupBudget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.yaml")
	if err := os.WriteFile(p, []byte("decision:\n  engines: [jev, llm]\n  backupBudget:\n    maxCalls: 100\n    per: 1h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.Decision.BackupBudget == nil || cfg.Decision.BackupBudget.MaxCalls != 100 || cfg.Decision.BackupBudget.Per != "1h" {
		t.Fatalf("%+v %v", cfg.Decision, err)
	}
}

// m4: rules belong in front of the engines.
func TestBuild_ADeterministicProviderAfterAnotherEngineIsRejected(t *testing.T) {
	rule := rules.New("rules", rules.Rule{Name: "x", Match: func(string, session.State) (decision.Decision, bool) { return decision.Decision{}, false }})
	deps, _, _ := budgetDeps()
	deps.Engines["rules"] = rule
	_, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "rules"} }, deps)
	if err == nil || !strings.Contains(err.Error(), "Deps.ExtraDecision") || !strings.Contains(err.Error(), `"rules"`) {
		t.Fatalf("%v", err)
	}
	// First in the engine list is allowed (nothing is ahead of it), and the usual
	// layout, rules in ExtraDecision, always is.
	if _, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"rules", "jev"} }, deps); err != nil {
		t.Fatal(err)
	}
	deps, _, _ = budgetDeps()
	deps.ExtraDecision = []decision.Provider{rule}
	if _, err := buildDecision(t, func(c *Config) { c.Decision.Engines = []string{"jev", "llm"} }, deps); err != nil {
		t.Fatal(err)
	}
}
