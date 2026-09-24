package rules

import (
	"context"
	"testing"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/session"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"  Show   my   Calendar!  ": "show my calendar",
		"Undo.":                     "undo",
		"CANCEL?":                   "cancel",
		"":                          "",
		"already normal":            "already normal",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPhrases(t *testing.T) {
	isCancel := Phrases("cancel", "never mind", "nevermind")
	if !isCancel(Normalize("Cancel.")) {
		t.Error("expected match")
	}
	if !isCancel(Normalize("  Never   Mind  ")) {
		t.Error("expected match with normalised whitespace")
	}
	if isCancel(Normalize("please cancel this")) {
		t.Error("Phrases must be exact match, not substring")
	}
}

func TestProvider_FirstRuleWins(t *testing.T) {
	calledSecond := false
	p := New("rules",
		Rule{Name: "cancel", Match: func(text string, st session.State) (decision.Decision, bool) {
			if Phrases("cancel")(text) {
				return decision.Decision{Module: decision.Scored{Value: "system", Confidence: 1}, Interaction: decision.InteractionCancellation, CanHandleDeterministically: true}, true
			}
			return decision.Decision{}, false
		}},
		Rule{Name: "anything", Match: func(text string, st session.State) (decision.Decision, bool) {
			calledSecond = true
			return decision.Decision{Module: decision.Scored{Value: "fallback", Confidence: 1}}, true
		}},
	)
	if p.Name() != "rules" {
		t.Fatalf("Name() = %q", p.Name())
	}
	d, ok, err := p.Decide(context.Background(), decision.Request{Text: "Cancel!"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ok || d.Module.Value != "system" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if calledSecond {
		t.Error("second rule must not run once the first matched")
	}
}

func TestProvider_AbstainsWhenNoRuleMatches(t *testing.T) {
	p := New("rules", Rule{Name: "cancel", Match: func(text string, st session.State) (decision.Decision, bool) {
		return decision.Decision{}, Phrases("cancel")(text)
	}})
	_, ok, err := p.Decide(context.Background(), decision.Request{Text: "what time is it"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if ok {
		t.Fatal("expected abstention")
	}
}

func TestProvider_NoRules(t *testing.T) {
	p := New("empty")
	_, ok, err := p.Decide(context.Background(), decision.Request{Text: "anything"})
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestProvider_CtxCancelledStopsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	p := New("rules", Rule{Name: "r", Match: func(text string, st session.State) (decision.Decision, bool) {
		called = true
		return decision.Decision{}, false
	}})
	_, ok, err := p.Decide(ctx, decision.Request{Text: "x"})
	if err == nil {
		t.Fatal("expected context error")
	}
	if ok {
		t.Fatal("expected no decision")
	}
	if called {
		t.Error("must not run rules once ctx is already cancelled")
	}
}

func TestProvider_UsesSessionState(t *testing.T) {
	p := New("rules", Rule{Name: "pronoun", Match: func(text string, st session.State) (decision.Decision, bool) {
		if text == "delete it" && st.Focused != nil {
			return decision.Decision{
				Module:    decision.Scored{Value: "calendar", Confidence: 1},
				Reference: &decision.Reference{Kind: st.Focused.Type, Pronoun: true},
			}, true
		}
		return decision.Decision{}, false
	}})
	focused := session.EntityRef{Type: "happening", Keys: map[string]string{"id": "1"}}
	d, ok, err := p.Decide(context.Background(), decision.Request{
		Text:  "Delete It",
		State: session.State{Focused: &focused},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ok || d.Reference == nil || d.Reference.Kind != "happening" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
}
