package decision

import (
	"errors"
	"fmt"
)

// SelectionPolicy turns a scored answer into an Outcome. It replaces scattered
// per-caller thresholds with one named, documented value, and it never picks
// "the highest score" blindly: a Choice must be confident AND clear of the
// runner-up, and an independent relevance list may legitimately select several
// candidates or none.
//
// A policy is applied only to CALIBRATED answers. An uncalibrated answer gets
// OutcomeUnscored whatever its numbers say.
//
// Use NarrowingPolicy or DurablePolicy, or build a value and Validate it. The
// zero value accepts everything and is not a sensible policy; Chain treats a
// nil *SelectionPolicy as "no policy" (the legacy MinConfidence behaviour).
type SelectionPolicy struct {
	// Name identifies the policy in traces.
	Name string `json:"name"`

	// KindChoice answers: the engine's own confidence must reach MinConfidence
	// (skipped when the engine reports none) and the top candidate must lead
	// the runner-up by at least MinGap. A top candidate equal to the
	// question's NoneID is OutcomeNone.
	MinConfidence float64 `json:"minConfidence"`
	MinGap        float64 `json:"minGap"`

	// KindRelevance answers: a candidate at or above MinProbability is
	// selected; at or above StrongProbability it is also "strong" (most
	// relevant); at or above PotentialProbability but below MinProbability it
	// is "potential" (kept for the trace, used only if validation needs it);
	// below that it is dropped.
	MinProbability       float64 `json:"minProbability"`
	StrongProbability    float64 `json:"strongProbability"`
	PotentialProbability float64 `json:"potentialProbability"`

	// MaxPicks caps how many candidates are selected (0 = no cap). When the cap
	// bites, the best are kept and the reason says so.
	MaxPicks int `json:"maxPicks,omitempty"`
}

// Documented defaults of the two named policies. They are starting values, to
// be calibrated on a product's own corpus and overridden per product or
// decision type; they live here so no caller hard-codes its own.
const (
	// Narrowing policy: read-only narrowing of a search space. A wrong answer
	// costs a wasted look, not a wrong fact.
	NarrowingMinConfidence        = 0.50
	NarrowingMinGap               = 0.20
	NarrowingMinProbability       = 0.60
	NarrowingStrongProbability    = 0.85
	NarrowingPotentialProbability = 0.30

	// Durable policy: writing knowledge that will be reused without asking
	// again. The bar is raised; callers should still add a deterministic check
	// or a person's confirmation.
	DurableMinConfidence        = 0.90
	DurableMinGap               = 0.20
	DurableMinProbability       = 0.90
	DurableStrongProbability    = 0.95
	DurablePotentialProbability = 0.60
)

// NarrowingPolicy is the default policy for read-only narrowing.
func NarrowingPolicy() SelectionPolicy {
	return SelectionPolicy{
		Name:                 "narrowing",
		MinConfidence:        NarrowingMinConfidence,
		MinGap:               NarrowingMinGap,
		MinProbability:       NarrowingMinProbability,
		StrongProbability:    NarrowingStrongProbability,
		PotentialProbability: NarrowingPotentialProbability,
	}
}

// DurablePolicy is the stricter policy for answers that become durable knowledge.
func DurablePolicy() SelectionPolicy {
	return SelectionPolicy{
		Name:                 "durable",
		MinConfidence:        DurableMinConfidence,
		MinGap:               DurableMinGap,
		MinProbability:       DurableMinProbability,
		StrongProbability:    DurableStrongProbability,
		PotentialProbability: DurablePotentialProbability,
	}
}

// Validate reports a misconfigured policy: every threshold in [0,1], thresholds
// ordered potential <= min <= strong, and MaxPicks not negative.
func (p SelectionPolicy) Validate() error {
	var errs []error
	for name, v := range map[string]float64{
		"minConfidence": p.MinConfidence, "minGap": p.MinGap,
		"minProbability": p.MinProbability, "strongProbability": p.StrongProbability,
		"potentialProbability": p.PotentialProbability,
	} {
		if v < 0 || v > 1 {
			errs = append(errs, fmt.Errorf("%s %.2f out of [0,1]", name, v))
		}
	}
	if p.PotentialProbability > p.MinProbability || p.MinProbability > p.StrongProbability {
		errs = append(errs, errors.New("thresholds must satisfy potentialProbability <= minProbability <= strongProbability"))
	}
	if p.MaxPicks < 0 {
		errs = append(errs, errors.New("maxPicks must not be negative"))
	}
	return errors.Join(errs...)
}

// Selection is a policy's verdict on one Answer.
type Selection struct {
	Outcome Outcome `json:"outcome"`
	// Picks are the selected candidate ids, best first (empty unless Selected
	// or Several).
	Picks []string `json:"picks,omitempty"`
	// Strong is the subset of Picks at or above the strong threshold
	// (relevance answers only).
	Strong []string `json:"strong,omitempty"`
	// Potential are candidates below the select threshold but worth showing
	// (relevance answers only).
	Potential []string `json:"potential,omitempty"`
	// Reason is a short machine-readable explanation of a non-selected
	// outcome, or of a truncation: not_calibrated, no_scores, none_of_these,
	// low_confidence, narrow_gap, nothing_above_floor, only_potential,
	// truncated_to_max_picks.
	Reason string `json:"reason,omitempty"`
}

// Reasons reported in Selection.Reason.
const (
	ReasonNotCalibrated  = "not_calibrated"
	ReasonNoScores       = "no_scores"
	ReasonNoneOfThese    = "none_of_these"
	ReasonLowConfidence  = "low_confidence"
	ReasonNarrowGap      = "narrow_gap"
	ReasonNothingAbove   = "nothing_above_floor"
	ReasonOnlyPotential  = "only_potential"
	ReasonTruncatedToMax = "truncated_to_max_picks"
)

// Evaluate applies the policy to one answer.
func (p SelectionPolicy) Evaluate(a Answer) Selection {
	if !a.Calibrated {
		return Selection{Outcome: OutcomeUnscored, Reason: ReasonNotCalibrated}
	}
	if len(a.Scores) == 0 {
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonNoScores}
	}
	// Scores may come from a caller that did not use NewAnswer; rank defensively.
	ranked := NewAnswer(a.QuestionID, a.Kind, a.Scores).Scores
	if a.Kind == KindRelevance {
		return p.evaluateRelevance(ranked)
	}
	return p.evaluateChoice(a, ranked)
}

func (p SelectionPolicy) evaluateChoice(a Answer, ranked []Score) Selection {
	top := ranked[0]
	if a.NoneID != "" && top.ID == a.NoneID {
		return Selection{Outcome: OutcomeNone, Reason: ReasonNoneOfThese}
	}
	if a.HasConfidence && a.Confidence < p.MinConfidence {
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonLowConfidence}
	}
	if len(ranked) > 1 && top.Probability-ranked[1].Probability < p.MinGap {
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonNarrowGap}
	}
	return Selection{Outcome: OutcomeSelected, Picks: []string{top.ID}}
}

func (p SelectionPolicy) evaluateRelevance(ranked []Score) Selection {
	var sel Selection
	for _, s := range ranked {
		switch {
		case s.Probability >= p.MinProbability:
			sel.Picks = append(sel.Picks, s.ID)
			if s.Probability >= p.StrongProbability {
				sel.Strong = append(sel.Strong, s.ID)
			}
		case s.Probability >= p.PotentialProbability:
			sel.Potential = append(sel.Potential, s.ID)
		}
	}
	if p.MaxPicks > 0 && len(sel.Picks) > p.MaxPicks {
		sel.Picks = sel.Picks[:p.MaxPicks]
		// Strong is a prefix of Picks (strong implies selected, both best first).
		sel.Strong = sel.Strong[:min(len(sel.Strong), p.MaxPicks)]
		sel.Reason = ReasonTruncatedToMax
	}
	switch {
	case len(sel.Picks) == 1:
		sel.Outcome = OutcomeSelected
	case len(sel.Picks) > 1:
		sel.Outcome = OutcomeSeveral
	case len(sel.Potential) > 0:
		sel.Outcome, sel.Reason = OutcomeUncertain, ReasonOnlyPotential
	default:
		sel.Outcome, sel.Reason = OutcomeNone, ReasonNothingAbove
	}
	return sel
}

// answerOf views a Decision's scores as one exclusive (Choice) answer so the
// same policy judges decisions and scored questions alike. The confidence is
// the Intent's (the Choice that picked it), else the Module's.
func answerOf(d Decision) Answer {
	scores := make([]Score, 0, len(d.Scores))
	for id, p := range d.Scores {
		scores = append(scores, Score{ID: id, Probability: p})
	}
	a := NewAnswer("decision", KindChoice, scores)
	a.Calibrated = d.Calibrated
	a.HasConfidence = true
	a.Confidence = d.Module.Confidence
	if d.Intent.Value != "" {
		a.Confidence = d.Intent.Confidence
	}
	return a
}

// EvaluateDecision applies the policy to a Decision that carries Scores.
func (p SelectionPolicy) EvaluateDecision(d Decision) Selection {
	return p.Evaluate(answerOf(d))
}
