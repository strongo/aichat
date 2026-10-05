package compose

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

func TestPolicyRefusalStopsFallbackAndLeavesBreakerClosed(t *testing.T) {
	primary := failing("hosted", decision.ErrPolicyRefusal)
	backup := instant("backup", "i")
	breaker := NewBreaker(primary, WithBreakerThreshold(1))
	engine := Fallback(breaker, backup, WithFallbackOn(OnQuota))
	_, ok, rep, err := engine.DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrPolicyRefusal) || rep.FallbackFired || rep.Attempts[0].Outcome != decision.AttemptPolicyRefusal || backup.calls.Load() != 0 || breaker.State() != BreakerClosed {
		t.Fatalf("ok=%v err=%v rep=%+v backup=%d breaker=%v", ok, err, rep, backup.calls.Load(), breaker.State())
	}
	// The same terminal classification applies to the scored route.
	scored := &fake{name: "scored", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, decision.ErrPolicyRefusal
	}}
	backupScored := &fake{name: "backup", score: func(context.Context, decision.ScoreRequest) (decision.ScoreResult, error) {
		return decision.ScoreResult{}, nil
	}}
	_, scoreRep, err := Fallback(scored, backupScored, WithFallbackOn(OnQuota)).ScoreTraced(context.Background(), scoreRequest())
	if !errors.Is(err, decision.ErrPolicyRefusal) || scoreRep.FallbackFired || scoreRep.Attempts[0].Outcome != decision.AttemptPolicyRefusal || backupScored.calls.Load() != 0 {
		t.Fatalf("score err=%v rep=%+v backup=%d", err, scoreRep, backupScored.calls.Load())
	}
}

func TestPolicyRefusalHaltsHedgeAndRaceWithOnQuota(t *testing.T) {
	noLeak(t)
	clk := newFakeClock()
	primary, refuse := quotaAfter("hosted", decision.ErrPolicyRefusal)
	backup := newGated("backup", "i")
	ch := startDecide(Hedged(primary, backup.fake, 100*time.Millisecond, WithClock(clk), WithFallbackOn(OnQuota)))
	fireHedge(t, clk, 100*time.Millisecond, backup.fake)
	close(refuse)
	out := <-ch
	if out.ok || !errors.Is(out.err, decision.ErrPolicyRefusal) || !out.rep.HedgeFired || out.rep.Attempts[0].Outcome != decision.AttemptPolicyRefusal {
		t.Fatalf("hedge: %+v", out)
	}
	close(backup.release)
	waitFor(t, func() bool { return backup.running.Load() == 0 }, "cancelled hedge backup")

	raceBackup := newGated("race-backup", "i")
	_, ok, rep, err := Race([]decision.Provider{failing("hosted", decision.ErrPolicyRefusal), raceBackup.fake}, WithFallbackOn(OnQuota)).DecideTraced(context.Background(), request())
	if ok || !errors.Is(err, decision.ErrPolicyRefusal) || rep.Attempts[0].Outcome != decision.AttemptPolicyRefusal {
		t.Fatalf("race: ok=%v err=%v rep=%+v", ok, err, rep)
	}
	close(raceBackup.release)
	waitFor(t, func() bool { return raceBackup.running.Load() == 0 }, "cancelled race backup")
}
