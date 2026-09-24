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

// fatal builds the fatal-pair item per the LLMProvider contract: an
// EventError event AND a non-nil Go error together, on the same yield.
func fatal(aiErr *Error) struct {
	ev  Event
	err error
} {
	return struct {
		ev  Event
		err error
	}{ev: Event{Type: EventError, Error: aiErr}, err: aiErr}
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

func TestCollect_FatalEventError(t *testing.T) {
	aiErr := &Error{Code: ErrCodeUpstream, Message: "down"}
	s := seq(
		ev(Event{Type: EventTextDelta, Text: "partial"}),
		fatal(aiErr),
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

func TestCollect_NonFatalEventErrorDoesNotTerminate(t *testing.T) {
	// Per the LLMProvider contract, an EventError with a nil Go error is
	// explicitly non-fatal: Collect must keep draining past it.
	s := seq(
		ev(Event{Type: EventTextDelta, Text: "a"}),
		ev(Event{Type: EventError, Error: &Error{Code: ErrCodeUpstream, Message: "transient"}}),
		ev(Event{Type: EventTextDelta, Text: "b"}),
		ev(Event{Type: EventCompleted}),
	)
	text, _, _, err := Collect(s)
	if err != nil {
		t.Fatalf("err = %v, want nil (non-fatal EventError must not surface as Collect's error)", err)
	}
	if text != "ab" {
		t.Errorf("text = %q, want %q (Collect must keep draining past a non-fatal EventError)", text, "ab")
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

// TestUsage_BillableTokens is the coordinator's follow-up regression test:
// CacheReadTokens/ReasoningTokens are informational SUBSETS of
// InputTokens/OutputTokens on "openai-compatible" (never added), but
// CacheReadTokens/CacheWriteTokens are ADDITIVE, billed-separately totals
// on "anthropic" (and the unrecognised-provider fallback).
func TestUsage_BillableTokens(t *testing.T) {
	u := Usage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  20,
		CacheWriteTokens: 5,
		ReasoningTokens:  10,
	}
	cases := []struct {
		provider string
		want     int64
	}{
		// InputTokens+OutputTokens only: CacheReadTokens/ReasoningTokens
		// are subsets already inside those two totals.
		{"openai-compatible", 150},
		// ai/openairesponses follows the same subset convention as
		// ai/openaicompat (see BillableTokens doc).
		{"openai-responses", 150},
		// InputTokens+OutputTokens+CacheReadTokens+CacheWriteTokens:
		// Anthropic bills cache reads/writes separately from input_tokens/
		// output_tokens. ReasoningTokens is never added (no adapter
		// populates it additively).
		{"anthropic", 175},
		// An unrecognised provider name falls back to the additive
		// (Anthropic-style) formula rather than silently dropping a
		// populated Cache*Tokens field.
		{"some-future-provider", 175},
		{"", 175},
	}
	for _, c := range cases {
		if got := u.BillableTokens(c.provider); got != c.want {
			t.Errorf("BillableTokens(%q) = %d, want %d", c.provider, got, c.want)
		}
	}
}

// TestUsage_BillableTokensZeroCacheFieldsMatchAcrossProviders covers the
// common case where no cache tokens are reported at all (a request that
// never hit the cache): both formulas must agree exactly.
func TestUsage_BillableTokensZeroCacheFieldsMatchAcrossProviders(t *testing.T) {
	u := Usage{InputTokens: 30, OutputTokens: 12}
	openai := u.BillableTokens("openai-compatible")
	responses := u.BillableTokens("openai-responses")
	anthropic := u.BillableTokens("anthropic")
	if openai != 42 || responses != 42 || anthropic != 42 {
		t.Errorf("openai-compatible = %d, openai-responses = %d, anthropic = %d, want all 42 with no cache tokens reported", openai, responses, anthropic)
	}
}
