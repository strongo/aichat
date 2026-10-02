package typesafe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
)

// m4: the abstention rate must be measurable, so an abstention says why.
func TestDecide_AbstentionCarriesANonSensitiveReasonCode(t *testing.T) {
	cases := map[string]struct {
		answers []string
		policy  *decision.SelectionPolicy
		want    string
	}{
		"intent other": {
			answers: []string{choiceJSON("intent", "other", 0.9, `"other":0.9,"calendar/show":0.1`), interactionAnswer("question", 0.9)},
			want:    AbstainIntentOther,
		},
		"low confidence": {
			answers: append(baseAnswers(), choiceJSON("interaction", "chat", 0.1, `"chat":0.4,"question":0.35,"command":0.25`)),
			want:    AbstainInteractionLowConfidence,
		},
		"narrow gap": {
			answers: append(baseAnswers(), choiceJSON("interaction", "chat", 0.9, `"chat":0.45,"question":0.4,"command":0.15`)),
			want:    AbstainInteractionGap,
		},
		"below the durable bar": {
			answers: append(baseAnswers(), choiceJSON("interaction", "confirmation", 0.8, `"confirmation":0.85,"command":0.15`)),
			want:    AbstainInteractionBelowDurable,
		},
		"an invalid policy": {
			answers: append(baseAnswers(), interactionAnswer("question", 0.9)),
			policy:  &decision.SelectionPolicy{},
			want:    "interaction_" + decision.ReasonInvalidPolicy,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeDoer{body: response(tc.answers...)}
			c := newClient(t, d)
			if tc.policy != nil {
				c.SetPolicy(*tc.policy)
			}
			dec, ok, rep, err := c.DecideTraced(context.Background(), decideRequest())
			if ok || err != nil || len(rep.Attempts) != 1 {
				t.Fatalf("dec=%+v ok=%v rep=%+v err=%v", dec, ok, rep, err)
			}
			a := rep.Attempts[0]
			if a.Provider != DefaultName || a.Outcome != decision.AttemptAbstained || a.Detail != tc.want || rep.Engine != DefaultName || rep.Model != testModel {
				t.Fatalf("attempt=%+v rep=%+v", a, rep)
			}
			if a.Usage == nil || a.Usage.InputTokens != 100 || a.Usage.OutputTokens != 20 {
				t.Fatalf("the billed call must be metered: %+v", a.Usage)
			}
			if strings.Contains(a.Detail, "calendar") || strings.Contains(a.Detail, "friday") {
				t.Fatalf("the reason leaks caller content: %q", a.Detail)
			}
			// And through a Chain, so it is in decision.Trace.
			pol := decision.NarrowingPolicy()
			c2 := newClient(t, &fakeDoer{body: response(tc.answers...)})
			if tc.policy != nil {
				c2.SetPolicy(*tc.policy)
			}
			_, ok, tr := decision.Chain{Providers: []decision.Provider{c2}, Policy: &pol}.Decide(context.Background(), decideRequest())
			if ok || len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != decision.AttemptAbstained || tr.Attempts[0].Detail != tc.want {
				t.Fatalf("ok=%v tr=%+v", ok, tr)
			}
		})
	}
}

// A decided answer and a failure each report one attempt: every upstream call
// is metered, not only the abstained ones (see usage_test.go for the usage).
func TestDecideTraced_DecidedAndFailedReportOneAttempt(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	dec, ok, rep, err := newClient(t, d).DecideTraced(context.Background(), decideRequest())
	if !ok || err != nil || len(rep.Attempts) != 1 || rep.Attempts[0].Outcome != decision.AttemptDecided || rep.Engine != DefaultName || rep.Model != testModel || dec.Model != testModel {
		t.Fatalf("dec=%+v ok=%v rep=%+v err=%v", dec, ok, rep, err)
	}
	// Direct use is unjudged: a decision from the provider is not actionable.
	if dec.Actionable() || dec.Provenance() != decision.ProvenanceCalibrated {
		t.Fatalf("%+v", dec)
	}
	_, ok, rep, err = newClient(t, &fakeDoer{err: errNetwork}).DecideTraced(context.Background(), decideRequest())
	if ok || err == nil || len(rep.Attempts) != 1 || rep.Attempts[0].Outcome != decision.AttemptError || rep.Attempts[0].Usage != nil {
		t.Fatalf("ok=%v rep=%+v err=%v", ok, rep, err)
	}
}

// m5: TypeSafe documents no error that tells an exhausted allowance from a rate
// limit, so every 429 stays a transient rate limit and is never decision.ErrQuota.
func TestA429IsAlwaysATransientRateLimitNeverAQuota(t *testing.T) {
	for name, body := range map[string]string{
		"empty":                "",
		"quota-looking type":   `{"error":{"type":"insufficient_quota","message":"out of credits"}}`,
		"rate-limit-looking":   `{"error":{"type":"rate_limit_error"}}`,
		"billing-looking text": `billing: credits exhausted`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(t, &fakeDoer{status: 429, body: body}).Ask(context.Background(), AskRequest{State: map[string]any{"a": 1}, Questions: map[string]Question{"q": Noul(map[string]any{"question": "x"})}})
			if !errors.Is(err, ErrRateLimited) || errors.Is(err, decision.ErrQuota) || errors.Is(err, decision.ErrMisconfigured) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
