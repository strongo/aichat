package decision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type delayedErr struct{ d time.Duration }

func (e delayedErr) Error() string             { return "rate limited" }
func (e delayedErr) RetryDelay() time.Duration { return e.d }

func TestRetryDelay(t *testing.T) {
	wrapped := fmt.Errorf("engine: %w", delayedErr{7 * time.Second})
	if got := RetryDelay(wrapped); got != 7*time.Second {
		t.Fatalf("got %v", got)
	}
	if got := RetryDelay(errors.New("plain")); got != 0 {
		t.Fatalf("plain: %v", got)
	}
	if got := RetryDelay(delayedErr{-time.Second}); got != 0 {
		t.Fatalf("negative: %v", got)
	}
	if got := RetryDelay(delayedErr{24 * time.Hour}); got != MaxRetryDelay {
		t.Fatalf("capped: %v", got)
	}
}

func TestInvalidDetailCarriesOnlyACount(t *testing.T) {
	err := Validate(Decision{Module: Scored{Value: "secret-module-name"}}, Taxonomy{})
	got := InvalidDetail(err)
	if strings.Contains(got, "secret") || !strings.Contains(got, "problem") {
		t.Fatalf("detail = %q", got)
	}
	if got := InvalidDetail(errors.New("one")); !strings.Contains(got, "(1 problem") {
		t.Fatalf("single = %q", got)
	}
}

func TestChain_ClassifiesAuthAndRejectedErrors(t *testing.T) {
	for want, err := range map[string]error{
		AttemptAuth:     fmt.Errorf("engine: %w", ErrAuth),
		AttemptRejected: fmt.Errorf("engine: %w", ErrInvalidRequest),
	} {
		p := providerFunc{name: "e", fn: func(_ context.Context, _ Request) (Decision, bool, error) { return Decision{}, false, err }}
		_, ok, tr := chainOf(nil, p).Decide(context.Background(), req())
		if ok || tr.Attempts[0].Outcome != want {
			t.Fatalf("%s: ok=%v tr=%+v", want, ok, tr)
		}
	}
}

func TestChain_InvalidAnswerDetailHasNoContent(t *testing.T) {
	bad := decided("secret-module", "show", 0.9)
	_, _, tr := chainOf(nil, constProvider("e", bad)).Decide(context.Background(), req())
	if d := tr.Attempts[0].Detail; tr.Attempts[0].Outcome != AttemptInvalid || strings.Contains(d, "secret") {
		t.Fatalf("attempt = %+v", tr.Attempts[0])
	}
}

func TestChain_TraceCarriesTheModel(t *testing.T) {
	d := decided("calendar", "show", 0.95)
	d.Model = "engine-1.2"
	_, ok, tr := Chain{Providers: []Provider{constProvider("e", d)}}.Decide(context.Background(), req())
	if !ok || tr.Model != "engine-1.2" {
		t.Fatalf("tr = %+v", tr)
	}
	// A traced provider's report names the model of the leaf that answered.
	stub := tracedStub{name: "fb", ok: true, d: d, rep: Report{Strategy: "fallback", Engine: "e", Model: "engine-9", Attempts: []Attempt{{Provider: "e", Outcome: AttemptDecided}}}}
	if _, _, tr := (Chain{Providers: []Provider{stub}}).Decide(context.Background(), req()); tr.Model != "engine-9" {
		t.Fatalf("tr = %+v", tr)
	}
}
