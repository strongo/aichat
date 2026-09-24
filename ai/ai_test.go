package ai

import (
	"encoding/json"
	"errors"
	"iter"
	"testing"
)

func seq(events ...struct {
	ev  Event
	err error
}) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for _, e := range events {
			if !yield(e.ev, e.err) {
				return
			}
		}
	}
}

func ev(e Event) struct {
	ev  Event
	err error
} {
	return struct {
		ev  Event
		err error
	}{ev: e}
}

func evErr(err error) struct {
	ev  Event
	err error
} {
	return struct {
		ev  Event
		err error
	}{err: err}
}

func TestCollect_TextAndUsage(t *testing.T) {
	s := seq(
		ev(Event{Type: EventStarted, Provider: "p", Model: "m"}),
		ev(Event{Type: EventTextDelta, Text: "Hello, "}),
		ev(Event{Type: EventTextDelta, Text: "world"}),
		ev(Event{Type: EventUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 5}}),
		ev(Event{Type: EventCompleted}),
	)
	text, structured, usage, err := Collect(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello, world" {
		t.Errorf("text = %q", text)
	}
	if structured != nil {
		t.Errorf("structured = %v, want nil", structured)
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestCollect_Structured(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	s := seq(
		ev(Event{Type: EventStarted}),
		ev(Event{Type: EventTextDelta, Text: `{"a":1}`}),
		ev(Event{Type: EventStructured, Structured: raw}),
		ev(Event{Type: EventCompleted}),
	)
	_, structured, _, err := Collect(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(structured) != string(raw) {
		t.Errorf("structured = %s, want %s", structured, raw)
	}
}

func TestCollect_CompletedUsageOverridesEarlier(t *testing.T) {
	s := seq(
		ev(Event{Type: EventUsage, Usage: &Usage{InputTokens: 1}}),
		ev(Event{Type: EventCompleted, Usage: &Usage{InputTokens: 2, OutputTokens: 9}}),
	)
	_, _, usage, err := Collect(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 2 || usage.OutputTokens != 9 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestCollect_StreamError(t *testing.T) {
	wantErr := errors.New("boom")
	s := seq(
		ev(Event{Type: EventTextDelta, Text: "partial"}),
		evErr(wantErr),
	)
	text, _, _, err := Collect(s)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if text != "partial" {
		t.Errorf("text = %q, want partial text kept before the error", text)
	}
}

func TestCollect_EventError(t *testing.T) {
	aiErr := &Error{Code: ErrCodeUpstream, Message: "down"}
	s := seq(
		ev(Event{Type: EventTextDelta, Text: "partial"}),
		ev(Event{Type: EventError, Error: aiErr}),
	)
	text, _, _, err := Collect(s)
	var got *Error
	if !errors.As(err, &got) || got != aiErr {
		t.Fatalf("err = %v, want %v", err, aiErr)
	}
	if text != "partial" {
		t.Errorf("text = %q", text)
	}
}

func TestCollect_EarlyBreak(t *testing.T) {
	called := 0
	s := func(yield func(Event, error) bool) {
		for i := 0; i < 5; i++ {
			called++
			if !yield(Event{Type: EventTextDelta, Text: "x"}, nil) {
				return
			}
		}
	}
	// Collect always drains to completion; verify a manual range-with-break
	// over the same generator (the pattern Collect relies on for the
	// contract) stops promptly, since interactive consumers rely on this.
	n := 0
	for range s {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if called != 2 {
		t.Fatalf("called = %d, want 2 (generator must stop when consumer breaks)", called)
	}
}

func TestError_ErrorString(t *testing.T) {
	e := &Error{Code: ErrCodeAuth, Message: "no key"}
	if e.Error() != "auth: no key" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestError_IsRetryable(t *testing.T) {
	var nilErr *Error
	if nilErr.IsRetryable() {
		t.Error("nil *Error must not be retryable")
	}
	if (&Error{Retryable: false}).IsRetryable() {
		t.Error("Retryable:false must report false")
	}
	if !(&Error{Retryable: true}).IsRetryable() {
		t.Error("Retryable:true must report true")
	}
}
