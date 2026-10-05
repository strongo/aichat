package decision

import (
	"context"
	"errors"
	"testing"
)

func TestChainPolicyRefusalStopsEvenWhenOtherRefusalsMayFallThrough(t *testing.T) {
	backup := llm(0.9)
	chain := Chain{
		Providers: []Provider{
			&countingProvider{name: "hosted", err: errorsJoin(ErrPolicyRefusal)},
			backup,
		},
		StopOnQuota: FallThrough, StopOnMisconfigured: FallThrough,
	}
	_, ok, trace := chain.Decide(context.Background(), req())
	if ok || backup.n.Load() != 0 || trace.StoppedBy != AttemptPolicyRefusal || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != AttemptPolicyRefusal || !errors.Is(trace.Err(), ErrPolicyRefusal) {
		t.Fatalf("ok=%v backup=%d trace=%+v err=%v", ok, backup.n.Load(), trace, trace.Err())
	}
}
