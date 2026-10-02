package decision

import (
	"testing"
	"time"
)

func TestMergeReport_KeepsTheEnginesUsageAndFillsWhatItLeftOut(t *testing.T) {
	engineUsage := &Usage{InputTokens: 10, OutputTokens: 2}
	judgedUsage := &Usage{InputTokens: 99, OutputTokens: 99}
	cases := []struct {
		name     string
		engine   Attempt
		judged   Attempt
		answered bool
		want     Attempt
	}{
		{
			name:     "a decided attempt keeps the engine's usage and latency, takes the verdict and the role",
			engine:   Attempt{Provider: "jev", Outcome: AttemptDecided, Latency: 5 * time.Millisecond, Usage: engineUsage},
			judged:   Attempt{Provider: "single(jev)", Outcome: AttemptUncertain, Detail: "uncertain: low_confidence", Latency: 9 * time.Millisecond, Role: "primary", Usage: judgedUsage},
			answered: true,
			want:     Attempt{Provider: "jev", Outcome: AttemptUncertain, Detail: "uncertain: low_confidence", Latency: 5 * time.Millisecond, Role: "primary", Usage: engineUsage},
		},
		{
			name:     "an engine that reported no usage or latency gets the caller's",
			engine:   Attempt{Provider: "jev", Outcome: AttemptDecided},
			judged:   Attempt{Provider: "single(jev)", Outcome: AttemptDecided, Latency: 9 * time.Millisecond, Role: "backup", Usage: judgedUsage},
			answered: true,
			want:     Attempt{Provider: "jev", Outcome: AttemptDecided, Latency: 9 * time.Millisecond, Role: "backup", Usage: judgedUsage},
		},
		{
			name:     "a failed engine keeps its usage and has its outcome classified by the caller",
			engine:   Attempt{Provider: "jev", Outcome: AttemptError, Detail: "typesafe: HTTP 401", Latency: 5 * time.Millisecond, Usage: engineUsage},
			judged:   Attempt{Provider: "jev", Outcome: AttemptAuth, Detail: "classified", Latency: 9 * time.Millisecond, Role: "primary"},
			answered: false,
			want:     Attempt{Provider: "jev", Outcome: AttemptAuth, Detail: "classified", Latency: 5 * time.Millisecond, Role: "primary", Usage: engineUsage},
		},
		{
			name:     "an abstention keeps its own reason; only the role is filled",
			engine:   Attempt{Provider: "jev", Outcome: AttemptAbstained, Detail: "intent_other", Usage: engineUsage},
			judged:   Attempt{Provider: "jev", Outcome: AttemptAbstained, Latency: 9 * time.Millisecond, Role: "primary"},
			answered: false,
			want:     Attempt{Provider: "jev", Outcome: AttemptAbstained, Detail: "intent_other", Latency: 9 * time.Millisecond, Role: "primary", Usage: engineUsage},
		},
		{
			name:     "the attempts of a combinator that failed are left alone",
			engine:   Attempt{Provider: "inner", Outcome: AttemptError, Detail: "own", Usage: engineUsage},
			judged:   Attempt{Provider: "fallback(inner,x)", Outcome: AttemptTimeout, Detail: "outer"},
			answered: false,
			want:     Attempt{Provider: "inner", Outcome: AttemptError, Detail: "own", Usage: engineUsage},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Report{Engine: "jev", Attempts: []Attempt{tc.engine}}
			got := MergeReport(rep, tc.judged, tc.answered)
			if len(got) != 1 || got[0].Provider != tc.want.Provider || got[0].Outcome != tc.want.Outcome || got[0].Detail != tc.want.Detail ||
				got[0].Latency != tc.want.Latency || got[0].Role != tc.want.Role || got[0].Usage != tc.want.Usage {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if rep.Attempts[0].Role == tc.want.Role && tc.want.Role != "" && tc.engine.Role == "" {
				t.Fatal("the report's own attempts were modified")
			}
		})
	}
}
