package decision

import (
	"errors"
	"fmt"
	"math"
)

// SelectionPolicy turns a scored answer into an Outcome. It replaces scattered
// per-caller thresholds with one named, documented value, and it never picks
// "the highest score" blindly: a Choice must be confident AND clear of the
// runner-up, and an independent relevance list may legitimately select several
// candidates or none.
//
// A policy is applied only to CALIBRATED answers. An uncalibrated answer gets
// OutcomeUnscored whatever its numbers say, with a PROPOSAL in
// Selection.Proposals (see Evaluate) that is never a selection. The one
// exception is a Decision (EvaluateDecision) under a policy whose
// AcceptUncalibratedAt is set: that opt-in names the self-reported confidence
// at which an uncalibrated decision is accepted (OutcomeAccepted).
//
// There is ONE rule for "may a caller act on this": Selection.Actionable and
// Decision.Actionable are true for a calibrated selection (selected, several)
// and for an explicitly accepted uncalibrated decision (accepted), and for
// nothing else.
//
// Use NarrowingPolicy or DurablePolicy, or build a value and Validate it. The
// zero value is invalid (it would select everything): Validate rejects it, and
// Evaluate refuses to select anything with an invalid policy. Chain treats a
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

	// AcceptUncalibratedAt is the explicit opt-in to acting on an UNCALIBRATED
	// decision (an LLM emulator's self-reported confidence, a deterministic
	// rule): a Decision whose Module and Intent confidences are both at or above
	// it is accepted (OutcomeAccepted, actionable); anything lower is unscored.
	// 0 (the zero value) means never: an uncalibrated decision is then only a
	// non-actionable proposal. It applies to decisions only; an uncalibrated
	// answer to a scored question is always a proposal (see Selection.Proposals).
	// DurablePolicy leaves it off; NarrowingPolicy sets it to
	// NarrowingAcceptUncalibratedAt. Without this opt-in, a calibrated engine
	// that goes down would silently lower the bar from the calibrated threshold
	// to an LLM's self-reported number.
	AcceptUncalibratedAt float64 `json:"acceptUncalibratedAt,omitempty"`
}

// Documented defaults of the two named policies. They are PROVISIONAL: they come
// from a single small measurement against one decision model version, they are
// not a calibration, and they MUST be re-measured on a product's own corpus and
// for the exact model id in use (a moved model alias moves the numbers; see
// typesafe.Config.Model) before anything depends on them. Override them per
// product or decision type through configuration; they live here so no caller
// hard-codes its own.
const (
	// Narrowing policy: choosing which candidates to examine first. A wrong
	// pick costs extra work, nothing more.
	NarrowingMinConfidence        = 0.50
	NarrowingMinGap               = 0.20
	NarrowingMinProbability       = 0.60
	NarrowingStrongProbability    = 0.85
	NarrowingPotentialProbability = 0.30
	// NarrowingAcceptUncalibratedAt is the self-reported confidence at which an
	// uncalibrated decision is accepted under the narrowing policy: the same 0.70
	// floor decision.Chain applies without a policy. A decision acted on at that
	// bar is a proposal for narrowing the work, never durable knowledge.
	NarrowingAcceptUncalibratedAt = 0.70

	// Durable policy: answers that will be stored and reused as fact. The bar
	// is higher; callers should still add a deterministic check
	// or a person's confirmation.
	DurableMinConfidence        = 0.90
	DurableMinGap               = 0.20
	DurableMinProbability       = 0.90
	DurableStrongProbability    = 0.95
	DurablePotentialProbability = 0.60
	// DurableAcceptUncalibratedAt is 0: the durable policy never acts on an
	// uncalibrated decision. A product that must may set AcceptUncalibratedAt.
	DurableAcceptUncalibratedAt = 0
)

// NarrowingPolicy is the default policy for narrowing a set of candidates.
func NarrowingPolicy() SelectionPolicy {
	return SelectionPolicy{
		Name:                 "narrowing",
		MinConfidence:        NarrowingMinConfidence,
		MinGap:               NarrowingMinGap,
		MinProbability:       NarrowingMinProbability,
		StrongProbability:    NarrowingStrongProbability,
		PotentialProbability: NarrowingPotentialProbability,
		AcceptUncalibratedAt: NarrowingAcceptUncalibratedAt,
	}
}

// DurablePolicy is the stricter policy for answers that will be stored and reused as fact.
func DurablePolicy() SelectionPolicy {
	return SelectionPolicy{
		Name:                 "durable",
		MinConfidence:        DurableMinConfidence,
		MinGap:               DurableMinGap,
		MinProbability:       DurableMinProbability,
		StrongProbability:    DurableStrongProbability,
		PotentialProbability: DurablePotentialProbability,
		AcceptUncalibratedAt: DurableAcceptUncalibratedAt,
	}
}

// AtLeast returns a policy at least as strict as both p and o: each threshold
// is the larger of the two, MaxPicks the smaller non-zero cap, and an
// uncalibrated decision is accepted only when BOTH accept it (the larger bar,
// and never when either never does). Its name is p's with "+strict" appended.
func (p SelectionPolicy) AtLeast(o SelectionPolicy) SelectionPolicy {
	r := p
	r.Name += "+strict"
	r.MinConfidence = max(p.MinConfidence, o.MinConfidence)
	r.MinGap = max(p.MinGap, o.MinGap)
	r.MinProbability = max(p.MinProbability, o.MinProbability)
	r.StrongProbability = max(p.StrongProbability, o.StrongProbability)
	r.PotentialProbability = max(p.PotentialProbability, o.PotentialProbability)
	switch {
	case p.MaxPicks == 0:
		r.MaxPicks = o.MaxPicks
	case o.MaxPicks != 0:
		r.MaxPicks = min(p.MaxPicks, o.MaxPicks)
	}
	if p.AcceptUncalibratedAt <= 0 || o.AcceptUncalibratedAt <= 0 {
		r.AcceptUncalibratedAt = 0
	} else {
		r.AcceptUncalibratedAt = max(p.AcceptUncalibratedAt, o.AcceptUncalibratedAt)
	}
	return r
}

// Validate reports a misconfigured policy: every threshold in [0,1],
// MinConfidence and MinProbability above zero (so the zero value, which would
// select everything, is invalid), thresholds ordered potential <= min <=
// strong, MaxPicks not negative and AcceptUncalibratedAt in [0,1].
func (p SelectionPolicy) Validate() error {
	var errs []error
	check := func(name string, v float64) {
		if math.IsNaN(v) || v < 0 || v > 1 {
			errs = append(errs, fmt.Errorf("%s %.2f out of [0,1]", name, v))
		}
	}
	check("minConfidence", p.MinConfidence)
	check("minGap", p.MinGap)
	check("minProbability", p.MinProbability)
	check("strongProbability", p.StrongProbability)
	check("potentialProbability", p.PotentialProbability)
	check("acceptUncalibratedAt", p.AcceptUncalibratedAt)
	if p.MinConfidence <= 0 || p.MinProbability <= 0 {
		errs = append(errs, errors.New("minConfidence and minProbability must be above 0 (a zero threshold selects everything)"))
	}
	if p.PotentialProbability > p.MinProbability || p.MinProbability > p.StrongProbability {
		errs = append(errs, errors.New("thresholds must satisfy potentialProbability <= minProbability <= strongProbability"))
	}
	if p.MaxPicks < 0 {
		errs = append(errs, errors.New("maxPicks must not be negative"))
	}
	return errors.Join(errs...)
}

// Selection is a policy's verdict on one Answer (or one Decision).
type Selection struct {
	Outcome Outcome `json:"outcome"`
	// Picks are the candidate ids the policy SELECTED, best first: non-empty
	// only for a calibrated selected or several verdict (and the decision key
	// for an accepted decision). Code that sees len(Picks) > 0 may act on them;
	// it never holds a proposal.
	Picks []string `json:"picks,omitempty"`
	// Proposals are the candidates an uncalibrated engine's self-reported numbers
	// point at (OutcomeUnscored): for a relevance answer the candidates at or
	// above MinProbability (capped by MaxPicks), for a choice answer its top
	// candidate unless that is the NoneID. A proposal is never a selection; a
	// caller may use it only where a wrong guess is cheap (for example choosing
	// which candidates to examine first, with the full set as the fallback).
	// Picks and Proposals are never both set.
	Proposals []string `json:"proposals,omitempty"`
	// Strong is the subset of Picks at or above the strong threshold
	// (relevance answers only).
	Strong []string `json:"strong,omitempty"`
	// Potential are candidates below the select threshold but worth showing
	// (relevance answers only).
	Potential []string `json:"potential,omitempty"`
	// Reason is a short machine-readable explanation of a non-selected
	// outcome, or of a truncation: not_calibrated, no_scores, none_of_these,
	// low_confidence, narrow_gap, nothing_above_floor, only_potential,
	// truncated_to_max_picks, invalid_policy, accepted_uncalibrated.
	Reason string `json:"reason,omitempty"`
}

// Detail is the verdict as a trace line: the outcome, and ": reason" when there
// is one ("uncertain: low_confidence", "accepted: accepted_uncalibrated").
func (s Selection) Detail() string {
	if s.Reason == "" {
		return string(s.Outcome)
	}
	return string(s.Outcome) + ": " + s.Reason
}

// Actionable reports whether a caller may act on the verdict: the policy
// selected a calibrated answer (OutcomeSelected, OutcomeSeveral) or accepted an
// uncalibrated decision at its explicit bar (OutcomeAccepted). Decision.Actionable
// applies the same rule.
func (s Selection) Actionable() bool { return s.Outcome.Actionable() }

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
	ReasonInvalidPolicy  = "invalid_policy"
	// ReasonAcceptedUncalibrated marks OutcomeAccepted: the policy's
	// AcceptUncalibratedAt opt-in, not a calibrated selection.
	ReasonAcceptedUncalibrated = "accepted_uncalibrated"
)

// Evaluate applies the policy to one answer.
//
// An invalid policy (see Validate) selects nothing: the outcome is
// OutcomeUncertain with ReasonInvalidPolicy. An uncalibrated answer is
// OutcomeUnscored with ReasonNotCalibrated and a PROPOSAL in Proposals (Picks,
// Strong and Potential stay empty): a relevance answer proposes the candidates
// at or above MinProbability (capped by MaxPicks), a choice answer proposes its
// top candidate unless that is the NoneID. A proposal lets a caller narrow a
// search space with an LLM engine's self-reported numbers; it is never a
// selection, and AcceptUncalibratedAt does not apply to it.
func (p SelectionPolicy) Evaluate(a Answer) Selection {
	if p.Validate() != nil {
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonInvalidPolicy}
	}
	if len(a.Scores) == 0 {
		if !a.Calibrated {
			return Selection{Outcome: OutcomeUnscored, Reason: ReasonNotCalibrated}
		}
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonNoScores}
	}
	// Scores may come from a caller that did not use NewAnswer; rank defensively.
	ranked := NewAnswer(a.QuestionID, a.Kind, a.Scores).Scores
	if !a.Calibrated {
		return p.propose(a, ranked)
	}
	if a.Kind == KindRelevance {
		return p.evaluateRelevance(ranked)
	}
	return p.evaluateChoice(a, ranked)
}

// propose builds the proposal for an uncalibrated answer.
func (p SelectionPolicy) propose(a Answer, ranked []Score) Selection {
	sel := Selection{Outcome: OutcomeUnscored, Reason: ReasonNotCalibrated}
	if a.Kind == KindRelevance {
		for _, s := range ranked {
			if s.Probability >= p.MinProbability {
				sel.Proposals = append(sel.Proposals, s.ID)
			}
		}
		if p.MaxPicks > 0 && len(sel.Proposals) > p.MaxPicks {
			sel.Proposals = sel.Proposals[:p.MaxPicks]
		}
		return sel
	}
	if top := ranked[0]; top.ID != a.NoneID {
		sel.Proposals = []string{top.ID}
	}
	return sel
}

func (p SelectionPolicy) evaluateChoice(a Answer, ranked []Score) Selection {
	top := ranked[0]
	if a.NoneID != "" && top.ID == a.NoneID {
		return Selection{Outcome: OutcomeNone, Reason: ReasonNoneOfThese}
	}
	// An engine that reports no confidence leaves the top probability as the only
	// evidence of certainty, so it must reach MinConfidence itself.
	confidence := top.Probability
	if a.HasConfidence {
		confidence = a.Confidence
	}
	if confidence < p.MinConfidence {
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

// EvaluateDecision applies the policy to a Decision. A calibrated decision with
// Scores is judged like a Choice (the policy's confidence and gap thresholds). A
// decision the policy cannot judge by probabilities (uncalibrated, or calibrated
// but without Scores) is OutcomeAccepted when AcceptUncalibratedAt is set and
// both its Module and Intent confidences reach it (the module confidence is
// exempt for a module-optional interaction, as in Chain), and OutcomeUnscored,
// which is not actionable, otherwise. An invalid policy selects nothing.
func (p SelectionPolicy) EvaluateDecision(d Decision) Selection {
	if p.Validate() != nil {
		return Selection{Outcome: OutcomeUncertain, Reason: ReasonInvalidPolicy}
	}
	if d.Calibrated && len(d.Scores) > 0 {
		return p.Evaluate(answerOf(d))
	}
	sel := Selection{Outcome: OutcomeUnscored, Reason: ReasonNotCalibrated}
	if d.Calibrated {
		sel.Reason = ReasonNoScores
	}
	switch {
	case p.AcceptUncalibratedAt <= 0:
	case lowConfidence(d, p.AcceptUncalibratedAt):
		sel.Reason = ReasonLowConfidence
	default:
		sel.Outcome, sel.Reason = OutcomeAccepted, ReasonAcceptedUncalibrated
		sel.Picks = []string{decisionKey(d)}
	}
	return sel
}

// decisionKey names a decision's pick as Decision.Scores does.
func decisionKey(d Decision) string {
	if d.Intent.Value == "" {
		return d.Module.Value
	}
	return d.Module.Value + "/" + d.Intent.Value
}
