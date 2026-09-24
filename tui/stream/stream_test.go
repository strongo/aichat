package stream

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai"
)

func seqOf(events ...ai.Event) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func seqWithError(events []ai.Event, err error) iter.Seq2[ai.Event, error] {
	return func(yield func(ai.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		yield(ai.Event{}, err)
	}
}

func drain(t *testing.T, cmd tea.Cmd, max int) ([]ai.Event, error) {
	t.Helper()
	var got []ai.Event
	for i := 0; i < max; i++ {
		msg := cmd()
		switch m := msg.(type) {
		case EventMsg:
			got = append(got, m.Event)
			cmd = m.Next
		case DoneMsg:
			return got, m.Err
		default:
			t.Fatalf("unexpected msg type %T", msg)
		}
	}
	t.Fatal("did not reach DoneMsg within max iterations")
	return nil, nil
}

func TestStartYieldsEventsInOrderThenDone(t *testing.T) {
	events := []ai.Event{
		{Type: ai.EventStarted, Provider: "test"},
		{Type: ai.EventTextDelta, Text: "hi"},
		{Type: ai.EventCompleted},
	}
	cmd := Start(context.Background(), "turn-1", seqOf(events...))
	got, err := drain(t, cmd, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d", len(got), len(events))
	}
	for i, ev := range events {
		if got[i].Type != ev.Type {
			t.Errorf("event %d type = %v, want %v", i, got[i].Type, ev.Type)
		}
	}
}

func TestStartPropagatesStreamError(t *testing.T) {
	boom := errors.New("boom")
	seq := seqWithError([]ai.Event{{Type: ai.EventStarted}}, boom)
	cmd := Start(context.Background(), "turn-1", seq)
	_, err := drain(t, cmd, 10)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestStartEmptySequenceYieldsDone(t *testing.T) {
	cmd := Start(context.Background(), "turn-1", seqOf())
	msg := cmd()
	done, ok := msg.(DoneMsg)
	if !ok {
		t.Fatalf("msg = %#v, want DoneMsg", msg)
	}
	if done.Err != nil || done.ID != "turn-1" {
		t.Fatalf("done = %+v", done)
	}
}

func TestStartCancellationYieldsDoneWithCtxErr(t *testing.T) {
	// A sequence that blocks forever until cancelled: it yields one event,
	// then would block on the second yield's write if the reader stops
	// reading, but our pump goroutine selects on ctx.Done() so it exits
	// promptly instead of leaking.
	block := make(chan struct{})
	seq := func(yield func(ai.Event, error) bool) {
		if !yield(ai.Event{Type: ai.EventStarted}, nil) {
			return
		}
		<-block // second event never arrives before cancellation
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := Start(ctx, "turn-1", seq)
	msg := cmd() // first EventMsg
	ev, ok := msg.(EventMsg)
	if !ok {
		t.Fatalf("msg = %#v, want EventMsg", msg)
	}
	cancel()
	done, ok := ev.Next().(DoneMsg)
	if !ok {
		t.Fatalf("msg after cancel = %#v, want DoneMsg", msg)
	}
	if !errors.Is(done.Err, context.Canceled) {
		t.Fatalf("done.Err = %v, want context.Canceled", done.Err)
	}
	close(block)
}

func TestStartCancellationWhileProducerBlockedOnSendStopsProducer(t *testing.T) {
	// Exercises the pump goroutine's own ctx.Done() case (the one guarding
	// `ch <- item{...}`), as opposed to next()'s ctx.Done() case. This needs
	// the goroutine to be caught AT that select, with ctx already cancelled
	// underneath it, and nobody consuming — otherwise we can't tell whether
	// it was that select or next()'s that produced the DoneMsg.
	//
	// We get that deterministically via the beforeSend test seam (stream.go)
	// instead of a sleep: beforeSend runs synchronously in the pump
	// goroutine right before it reaches the select, so blocking there until
	// the test releases it guarantees the goroutine has not yet attempted
	// the send when we cancel ctx. Cancelling before release means that by
	// the time the goroutine's select finally evaluates, ctx.Done() is
	// already the only ready case (nothing is receiving from ch), so it is
	// the one deterministically chosen — no race, no timing dependency.
	reachedSecondSend := make(chan struct{})
	releaseSecondSend := make(chan struct{})
	calls := 0
	orig := beforeSend
	t.Cleanup(func() { beforeSend = orig })
	beforeSend = func() {
		calls++
		if calls == 2 {
			close(reachedSecondSend)
			<-releaseSecondSend
		}
	}

	seq := seqOf(
		ai.Event{Type: ai.EventStarted},
		ai.Event{Type: ai.EventTextDelta, Text: "second"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := Start(ctx, "turn-1", seq)
	msg := cmd() // consumes the first event
	ev, ok := msg.(EventMsg)
	if !ok {
		t.Fatalf("msg = %#v, want EventMsg", msg)
	}

	<-reachedSecondSend // goroutine is blocked in beforeSend, before its select
	cancel()            // ctx.Done() closes while nobody can be mid-send
	close(releaseSecondSend)

	done, ok := ev.Next().(DoneMsg)
	if !ok {
		t.Fatalf("msg after cancel = %#v, want DoneMsg", msg)
	}
	if !errors.Is(done.Err, context.Canceled) {
		t.Fatalf("done.Err = %v, want context.Canceled", done.Err)
	}
}

func TestNextTimesOutWithoutLeakingWhenNoConsumer(t *testing.T) {
	// Regression guard: Start must not block forever if nobody ever reads
	// the returned tea.Cmd (e.g. a test that only cares about compilation).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	seq := func(yield func(ai.Event, error) bool) {
		<-ctx.Done()
	}
	cmd := Start(ctx, "t", seq)
	msg := cmd()
	if _, ok := msg.(DoneMsg); !ok {
		t.Fatalf("msg = %#v, want DoneMsg", msg)
	}
}
