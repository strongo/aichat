package aiconfig

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
)

// splitList splits a comma-separated list, trimming blanks and dropping empties.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// policyByName resolves a named policy.
func policyByName(name string) (*decision.SelectionPolicy, error) {
	switch name {
	case "":
		return nil, nil
	case "narrowing":
		p := decision.NarrowingPolicy()
		return &p, nil
	case "durable":
		p := decision.DurablePolicy()
		return &p, nil
	default:
		return nil, fmt.Errorf("aiconfig: unknown decision.policy %q (want narrowing or durable)", name)
	}
}

// policyFromConfig resolves Decision.Policy and applies Decision.PolicyValues
// on top, then validates the result.
func policyFromConfig(cfg Decision) (*decision.SelectionPolicy, error) {
	p, err := policyByName(cfg.Policy)
	if err != nil {
		return nil, err
	}
	v := cfg.PolicyValues
	if v == nil {
		return p, nil
	}
	if p == nil {
		return nil, errors.New("aiconfig: decision.policyValues needs decision.policy to name the policy to override")
	}
	set := func(dst *float64, src *float64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&p.MinConfidence, v.MinConfidence)
	set(&p.MinGap, v.MinGap)
	set(&p.MinProbability, v.MinProbability)
	set(&p.StrongProbability, v.StrongProbability)
	set(&p.PotentialProbability, v.PotentialProbability)
	set(&p.AcceptUncalibratedAt, v.AcceptUncalibratedAt)
	if v.MaxPicks != nil {
		p.MaxPicks = *v.MaxPicks
	}
	if v.AcceptUncalibratedSideEffects != nil {
		p.AcceptUncalibratedSideEffects = *v.AcceptUncalibratedSideEffects
	}
	p.Name += "+custom"
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("aiconfig: decision.policyValues: %w", err)
	}
	return p, nil
}

// stopFromConfig maps an optional bool (nil: the default, ON) to a decision.Stop.
func stopFromConfig(v *bool) decision.Stop {
	if v != nil && !*v {
		return decision.StopOff
	}
	return decision.StopOn
}

// buildEngine assembles the configured decision engines into one
// decision.Provider, or returns nil when none are configured.
func buildEngine(cfg Decision, deps Deps, policy *decision.SelectionPolicy) (decision.Provider, error) {
	if len(cfg.Engines) == 0 {
		return nil, nil
	}
	breaker := cfg.Breaker == nil || *cfg.Breaker
	providers := make([]decision.Provider, len(cfg.Engines))
	for i, name := range cfg.Engines {
		p, ok := deps.Engines[name]
		if !ok {
			return nil, fmt.Errorf("aiconfig: decision.engines names %q but Deps.Engines has no such engine", name)
		}
		if breaker {
			bopts := []compose.BreakerOption{compose.WithBreakerOnChange(deps.OnBreakerChange), compose.WithBreakerSlowThreshold(cfg.BreakerSlowThreshold)}
			if deps.Clock != nil {
				bopts = append(bopts, compose.WithBreakerClock(deps.Clock))
			}
			p = compose.NewBreaker(p, bopts...)
		}
		providers[i] = p
	}

	strategy := compose.Strategy(cfg.Strategy)
	if strategy == "" {
		strategy = compose.StrategySingle
		if len(providers) == 2 {
			strategy = compose.StrategyFallback
		}
	}
	opts := []compose.Option{}
	if deps.Clock != nil {
		opts = append(opts, compose.WithClock(deps.Clock))
	}
	also, err := fallbackTriggers(cfg.FallbackOn)
	if err != nil {
		return nil, err
	}
	opts = append(opts, compose.WithFallbackOn(also))
	if policy != nil {
		opts = append(opts, compose.WithPolicy(*policy))
	}

	need := func(n int) error {
		if len(providers) != n {
			return fmt.Errorf("aiconfig: decision.strategy %q needs exactly %d engine(s), got %d", strategy, n, len(providers))
		}
		return nil
	}
	switch strategy {
	case compose.StrategySingle:
		if err := need(1); err != nil {
			return nil, err
		}
		return compose.Single(providers[0], opts...), nil
	case compose.StrategyFallback:
		if err := need(2); err != nil {
			return nil, err
		}
		return compose.Fallback(providers[0], providers[1], opts...), nil
	case compose.StrategyHedged:
		if err := need(2); err != nil {
			return nil, err
		}
		after := compose.DefaultHedgeAfter
		if cfg.HedgeAfter != "" {
			d, err := time.ParseDuration(cfg.HedgeAfter)
			if err != nil || d < 0 {
				return nil, fmt.Errorf("aiconfig: decision.hedgeAfter %q is not a non-negative duration", cfg.HedgeAfter)
			}
			after = d
		}
		return compose.Hedged(providers[0], providers[1], after, opts...), nil
	case compose.StrategyRace:
		if len(providers) < 2 {
			return nil, fmt.Errorf("aiconfig: decision.strategy %q needs at least 2 engines, got %d", strategy, len(providers))
		}
		return compose.Race(providers, opts...), nil
	default:
		return nil, fmt.Errorf("aiconfig: unknown decision.strategy %q (want single, fallback, hedged or race)", cfg.Strategy)
	}
}

func fallbackTriggers(names []string) (compose.Trigger, error) {
	var t compose.Trigger
	for _, n := range names {
		switch n {
		case "abstain":
			t |= compose.OnAbstain
		case "uncertain":
			t |= compose.OnUncertain
		case "quota":
			t |= compose.OnQuota
		default:
			return 0, fmt.Errorf("aiconfig: unknown decision.fallbackOn %q (want abstain, uncertain or quota)", n)
		}
	}
	return t, nil
}
