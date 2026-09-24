package decision

import (
	"context"
	"errors"
	"fmt"
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
		Module:      Scored{Value: module, Confidence: conf},
		Intent:      Scored{Value: intent, Confidence: conf},
		Interaction: InteractionCommand,
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

func TestValidate_InteractionRequired(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.Interaction = ""
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "interaction is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_UnknownInteraction(t *testing.T) {
	d := decided("calendar", "show", 0.9)
	d.Interaction = Interaction("nope")
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown interaction") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate_ModuleOptionalForConfirmationEtc(t *testing.T) {
	for _, ia := range []Interaction{InteractionConfirmation, InteractionRejection, InteractionCancellation, InteractionUndo} {
		d := Decision{Interaction: ia}
		if err := Validate(d, taxonomy()); err != nil {
			t.Errorf("interaction %q: unexpected error with empty module: %v", ia, err)
		}
	}
}

func TestValidate_ModuleStillRequiredForOtherInteractions(t *testing.T) {
	d := Decision{Interaction: InteractionCommand}
	err := Validate(d, taxonomy())
	if err == nil || !strings.Contains(err.Error(), "unknown module") {
		t.Fatalf("err = %v, want unknown module for a non-exempt interaction with empty Module", err)
	}
}

func TestValidate_ReferenceKindAgainstEntityTypes(t *testing.T) {
	tx := taxonomy()
	tx.EntityTypes = []string{"happening"}
	d := decided("calendar", "show", 0.9)
	d.Reference = &Reference{Kind: "unknown_entity"}
	err := Validate(d, tx)
	if err == nil || !strings.Contains(err.Error(), "unknown reference kind") {
		t.Fatalf("err = %v", err)
	}
	d.Reference.Kind = "happening"
	if err := Validate(d, tx); err != nil {
		t.Fatalf("unexpected error for a known reference kind: %v", err)
	}
}

func TestValidate_RequiredDataAgainstDataKinds(t *testing.T) {
	tx := taxonomy()
	tx.DataKinds = []string{"relevant_happenings"}
	d := decided("calendar", "show", 0.9)
	d.RequiredData = []string{"nope"}
	err := Validate(d, tx)
	if err == nil || !strings.Contains(err.Error(), "unknown required data kind") {
		t.Fatalf("err = %v", err)
	}
	d.RequiredData = []string{"relevant_happenings"}
	if err := Validate(d, tx); err != nil {
		t.Fatalf("unexpected error for a known data kind: %v", err)
	}
}

func TestChain_ModuleOptionalDecisionAcceptedAtAnyConfidence(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return Decision{Interaction: InteractionConfirmation}, true, nil
	}}
	c := Chain{Providers: []Provider{p1}}
	d, ok, tr := c.Decide(context.Background(), req())
	if !ok {
		t.Fatalf("expected a module-optional confirmation to be accepted: tr=%+v", tr)
	}
	if d.Interaction != InteractionConfirmation {
		t.Fatalf("d = %+v", d)
	}
}

func TestChain_MinConfidenceNegativeAcceptsAny(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.01), true, nil
	}}
	c := Chain{Providers: []Provider{p1}, MinConfidence: -1}
	_, ok, tr := c.Decide(context.Background(), req())
	if !ok {
		t.Fatalf("MinConfidence<0 must accept any confidence: tr=%+v", tr)
	}
}

func TestChain_MinConfidenceZeroDefaultsTo07(t *testing.T) {
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		return decided("calendar", "show", 0.5), true, nil
	}}
	c := Chain{Providers: []Provider{p1}} // MinConfidence zero value
	_, ok, tr := c.Decide(context.Background(), req())
	if ok {
		t.Fatalf("default MinConfidence 0.7 must reject a 0.5 decision: tr=%+v", tr)
	}
}

// timeoutProbeProvider reports the ctx deadline it was actually given, so a
// test can assert Chain honoured DecisionTimeout() instead of the chain
// default.
type timeoutProbeProvider struct {
	name    string
	timeout time.Duration
	got     chan time.Duration
}

func (p timeoutProbeProvider) Name() string                   { return p.name }
func (p timeoutProbeProvider) DecisionTimeout() time.Duration { return p.timeout }
func (p timeoutProbeProvider) Decide(ctx context.Context, r Request) (Decision, bool, error) {
	dl, _ := ctx.Deadline()
	p.got <- time.Until(dl)
	return Decision{}, false, nil
}

func TestChain_HonoursProviderDecisionTimeout(t *testing.T) {
	got := make(chan time.Duration, 1)
	p := timeoutProbeProvider{name: "p", timeout: 4 * time.Second, got: got}
	c := Chain{Providers: []Provider{p}, Timeout: 50 * time.Millisecond}
	_, _, _ = c.Decide(context.Background(), req())
	d := <-got
	if d < 3*time.Second {
		t.Fatalf("provider's actual deadline was %v, want close to its own 4s DecisionTimeout (chain default is 50ms)", d)
	}
}

func TestChain_TimeoutDetectionUsesErrorsIs(t *testing.T) {
	// A provider that wraps context.DeadlineExceeded must still be
	// classified as "timeout", not "error", per R3 (errors.Is, not ==).
	p1 := providerFunc{"p1", func(ctx context.Context, r Request) (Decision, bool, error) {
		<-ctx.Done()
		return Decision{}, false, fmt.Errorf("wrapped: %w", ctx.Err())
	}}
	c := Chain{Providers: []Provider{p1}, Timeout: 10 * time.Millisecond}
	_, ok, tr := c.Decide(context.Background(), req())
	if ok {
		t.Fatal("expected no decision")
	}
	if tr.Attempts[0].Outcome != "timeout" {
		t.Fatalf("Attempts[0] = %+v, want outcome=timeout", tr.Attempts[0])
	}
}
