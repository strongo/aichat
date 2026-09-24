package stream

import (
	"context"
	"iter"

	tea "charm.land/bubbletea/v2"

	"github.com/strongo/aichat/ai"
)

// EventMsg is one normalised stream event. Next re-arms the pump: a handler
// must return it as (part of) its tea.Cmd to receive the following event.
type EventMsg struct {
	ID    string
	Event ai.Event
	Next  tea.Cmd
}

// DoneMsg ends a stream started with Start: seq was fully drained (Err nil),
// it yielded an error (Err set to that error), or ctx was cancelled (Err set
// to ctx.Err()).
type DoneMsg struct {
	ID  string
	Err error
}

type item struct {
	event ai.Event
	err   error
}

// beforeSend, when non-nil, is invoked synchronously by Start's pump
// goroutine right before each attempt to hand an item to the consumer (i.e.
// immediately before the `select` guarding `ch <- item{...}`). It exists
// purely as a test seam so a test can deterministically synchronize with
// the goroutine at that exact point instead of relying on a sleep; it is
// nil (a no-op) in production.
var beforeSend func()

// Start begins draining seq in a goroutine — never buffering the whole
// response — and returns the tea.Cmd producing the first message. ctx
// cancellation stops the goroutine promptly and the pump.
//
// For a cancellation to reach the underlying provider request (not just stop
// this local pump), seq must itself have been built from ctx — e.g.
// provider.Stream(ctx, req) — so an adapter observing ctx.Done() can abort
// its own HTTP call and yield ai.ErrCodeCanceled. chatshell.Model.StartStream
// does this: it hands its per-stream ctx to the caller's seq-opening func
// before calling Start with that same ctx.
func Start(ctx context.Context, id string, seq iter.Seq2[ai.Event, error]) tea.Cmd {
	ch := make(chan item)
	go func() {
		defer close(ch)
		for ev, err := range seq {
			if beforeSend != nil {
				// Test-only seam: lets a test deterministically synchronize
				// with the pump goroutine immediately before it attempts to
				// hand an item to the consumer, instead of sleeping. See
				// stream_test.go.
				beforeSend()
			}
			select {
			case ch <- item{event: ev, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return next(ctx, id, ch)
}

func next(ctx context.Context, id string, ch <-chan item) tea.Cmd {
	return func() tea.Msg {
		select {
		case it, ok := <-ch:
			if !ok {
				return DoneMsg{ID: id}
			}
			if it.err != nil {
				return DoneMsg{ID: id, Err: it.err}
			}
			return EventMsg{ID: id, Event: it.event, Next: next(ctx, id, ch)}
		case <-ctx.Done():
			return DoneMsg{ID: id, Err: ctx.Err()}
		}
	}
}
