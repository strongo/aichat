package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/session"
)

func taxonomy() Taxonomy {
	return Taxonomy{
		Modules: []ModuleSpec{
			{Name: "calendar", Intents: []string{"show", "create"}, Scopes: []string{"calendar", "calendar.happenings"}},
		},
		Presentations: []string{"day_calendar"},
	}
}

func decided(module, intent string, conf float64) Decision {
	return Decision{
		Module: Scored{Value: module, Confidence: conf},
		Intent: Scored{Value: intent, Confidence: conf},
	}
}

// providerFunc adapts a function to Provider.
type providerFunc struct {
	name string
	fn   func(ctx context.Context, req Request) (Decision, bool, error)
}

func (p providerFunc) Name() string { return p.name }
func (p providerFunc) Decide(ctx context.Context, req Request) (Decision, bool, error) {
	return p.fn(ctx, req)
}

func TestValidate_OK(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.RequiredScopes = []string{"calendar.happenings"}
	d.Presentation = "day_calendar"
	if err := Validate(d, taxonomy()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_UnknownModule(t *testing.T) {
	d := decided("unknown_module", "", 0.9)
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_UnknownIntent(t *testing.T) {
	d := decided("calendar", "nope", 0.9)
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown intent") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_ConfidenceOutOfRange(t *testing.T) {
	d := decided("calendar", "show", 1.5)
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "confidence out of range") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_UnknownPresentation(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.Presentation = "nope"
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown presentation") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_UnknownScope(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.RequiredScopes = []string{"nope"}
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_ModuleNameIsAlsoAScope(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.RequiredScopes = []string{"calendar"}
	if err := Validate(d, taxonomy()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func req() Request {
	return Request{Product: "sneat", Text: "show my calendar", Taxonomy: taxonomy(), Now: time.Now()}
}

func TestChain_FirstDeciderWins(t *testing.T) {
	called2 := false
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		called2 = true
		return decided("calendar", "create", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok {
		t.Fatal("expected decision")
	}
	if d.Intent.Value != "show" {
		t.Fatalf("d.Intent = %v, want p1's decision", d.Intent)
	}
	if called2 {
		t.Fatal("second provider must not run once the first decided")
	}
	if tr.DecidedBy != "p1" {
		t.Fatalf("DecidedBy = %q", tr.DecidedBy)
	}
	if len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != "decided" {
		t.Fatalf("Attempts = %+v", tr.Attempts)
	}
}

func TestChain_AbstainFallsThrough(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return Decision{}, false, nil
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Intent.Value != "show" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if tr.Attempts[0].Outcome != "abstained" {
		t.Fatalf("Attempts[0] = %+v", tr.Attempts[0])
	}
	if tr.DecidedBy != "p2" {
		t.Fatalf("DecidedBy = %q", tr.DecidedBy)
	}
}

func TestChain_ErrorFallsThrough(t *testing.T) {
	wantErr := errors.New("boom")
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return Decision{}, false, wantErr
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Intent.Value != "show" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if tr.Attempts[0].Outcome != "error" || tr.Attempts[0].Detail != wantErr.Error() {
		t.Fatalf("Attempts[0] = %+v", tr.Attempts[0])
	}
}

func TestChain_TimeoutFallsThrough(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		<-ctx.Done()
		return Decision{}, false, ctx.Err()
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}, Timeout: 10 * time.Millisecond}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Intent.Value != "show" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if tr.Attempts[0].Outcome != "timeout" {
		t.Fatalf("Attempts[0] = %+v", tr.Attempts[0])
	}
}

func TestChain_InvalidFallsThrough(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("nope_module", "show", 0.9), true, nil
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Module.Value != "calendar" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if tr.Attempts[0].Outcome != "invalid" {
		t.Fatalf("Attempts[0] = %+v", tr.Attempts[0])
	}
}

func TestChain_LowConfidenceFallsThrough(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.3), true, nil
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok || d.Module.Confidence != 0.9 {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if tr.Attempts[0].Outcome != "low_confidence" {
		t.Fatalf("Attempts[0] = %+v", tr.Attempts[0])
	}
}

func TestChain_AllAbstainNoError(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return Decision{}, false, nil
	}}
	c := Chain{Providers: []Provider{p1}}
	_, ok, tr := c.Decide(context.Background(), req())
	if ok {
		t.Fatal("expected no decision")
	}
	if tr.DecidedBy != "" {
		t.Fatalf("DecidedBy = %q, want empty", tr.DecidedBy)
	}
}

func TestChain_EmptyChain(t *testing.T) {
	c := Chain{}
	_, ok, tr := c.Decide(context.Background(), req())
	if ok || len(tr.Attempts) != 0 {
		t.Fatalf("ok=%v tr=%+v", ok, tr)
	}
}

func TestChain_ParentCancelStopsChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called2 := false
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return Decision{}, false, ctx.Err()
	}}
	p2 := providerFunc{"p2", func(ctx context.Context, r Request) (Decision, bool, error) {
		called2 = true
		return decided("calendar", "show", 0.9), true, nil
	}}
	c := Chain{Providers: []Provider{p1, p2}}
	_, ok, _ := c.Decide(ctx, req())
	if ok {
		t.Fatal("expected no decision once parent ctx is cancelled")
	}
	if called2 {
		t.Fatal("chain must stop once the parent context is done")
	}
}

// session.State is used by Request; a smoke check it wires through.
func TestRequest_State(t *testing.T) {
	r := req()
	r.State = session.State{}
	if r.State.Focused != nil {
		t.Fatalf("State.Focused = %v", r.State.Focused)
	}
}
