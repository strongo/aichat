package decision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// Outcome is a SelectionPolicy's verdict on a scored answer. Callers act on
// the Outcome, never on a probability compared against a number of their own.
type Outcome string

const (
	// OutcomeSelected: one clear answer.
	OutcomeSelected Outcome = "selected"
	// OutcomeSeveral: more than one candidate is clearly relevant; keep all.
	OutcomeSeveral Outcome = "several"
	// OutcomeUncertain: no clear answer; escalate (next rung, or ask a person).
	OutcomeUncertain Outcome = "uncertain"
	// OutcomeNone: the engine says none of the candidates fits.
	OutcomeNone Outcome = "none"
	// OutcomeUnscored: the engine produced no calibrated probabilities (an LLM
	// emulator), so no threshold can be applied. The answer is a proposal, never a
	// "clear winner by margin", and it is NOT actionable.
	OutcomeUnscored Outcome = "unscored"
	// OutcomeAccepted: an UNCALIBRATED decision accepted because the caller's
	// policy explicitly opted in (SelectionPolicy.AcceptUncalibratedAt) and its
	// self-reported confidence reached that bar. It is actionable, and it is
	// never a calibrated selection: Decision.Calibrated stays false.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeDeterministic: the decision came from exact logic (a rule table; see
	// Deterministic), so there is no estimate to threshold. It is actionable under
	// every policy, and no AcceptUncalibratedAt bar applies to it.
	OutcomeDeterministic Outcome = "deterministic"
	// OutcomeFloor: a Chain WITHOUT a SelectionPolicy accepted the decision at its
	// MinConfidence floor, the caller's own explicit bar. It is the explicit
	// verdict that replaces "actionable because nothing judged it".
	OutcomeFloor Outcome = "floor"
	// OutcomeInvalid: the decision contradicts itself (its own intent is not the
	// top of its own Scores, or is missing from them). It is NOT actionable, and a
	// chain records it as an invalid answer and falls through.
	OutcomeInvalid Outcome = "invalid"
)

// Actionable reports whether an outcome lets a caller act: a calibrated
// selection (OutcomeSelected, OutcomeSeveral), an explicitly accepted
// uncalibrated decision (OutcomeAccepted), a deterministic decision
// (OutcomeDeterministic) or one a policy-less chain accepted at its floor
// (OutcomeFloor). Uncertain, none, unscored, invalid and the empty outcome
// (nothing judged it) are not.
func (o Outcome) Actionable() bool {
	switch o {
	case OutcomeSelected, OutcomeSeveral, OutcomeAccepted, OutcomeDeterministic, OutcomeFloor:
		return true
	}
	return false
}

// QuestionKind says how the probabilities of one Question relate to each other.
type QuestionKind string

const (
	// KindChoice: exactly one candidate is right; probabilities sum to 1.
	// "Which ONE of these?"
	KindChoice QuestionKind = "choice"
	// KindRelevance: each candidate is judged on its own; probabilities are
	// independent and need not sum to 1. "Which of these are relevant?"
	KindRelevance QuestionKind = "relevance"
)

// Candidate is one option of a Question.
type Candidate struct {
	ID string `json:"id"`
	// Description is optional public text that helps the engine understand the
	// candidate (for example a table's field names). Engines perform much
	// better with it than with a bare name.
	Description string `json:"description,omitempty"`
}

// Question asks an engine to score a closed set of candidates.
type Question struct {
	// ID names the question within a ScoreRequest; answers come back under it.
	ID           string       `json:"id"`
	Kind         QuestionKind `json:"kind"`
	Instructions string       `json:"instructions"`
	Candidates   []Candidate  `json:"candidates"`
	// NoneID, when it names one of Candidates, is the "none of these" option
	// of a KindChoice question: choosing it yields OutcomeNone.
	NoneID string `json:"noneId,omitempty"`
}

// ScoreRequest is the input to ScoredProvider.Score: the state the questions
// are asked about, and the questions. Several independent questions travel in
// one request so an engine can answer them in one round trip.
type ScoreRequest struct {
	Product string `json:"product,omitempty"`
	// Text is the primary state (for example the user's question).
	Text string `json:"text"`
	// Context is optional JSON-able context: names and public metadata only,
	// never row data or secrets.
	Context   map[string]any `json:"context,omitempty"`
	Questions []Question     `json:"questions"`
}

// Score is one candidate's probability.
type Score struct {
	ID          string  `json:"id"`
	Probability float64 `json:"probability"`
}

// Answer is an engine's scores for one Question.
type Answer struct {
	QuestionID string       `json:"questionId"`
	Kind       QuestionKind `json:"kind"`
	// Scores are sorted by probability descending, ties by ID ascending.
	Scores []Score `json:"scores"`
	// Confidence in [0,1] is the engine's own certainty about the whole answer
	// (for a Choice, derived from the distribution). It is only meaningful
	// when HasConfidence is true: an independent relevance answer has none.
	Confidence    float64 `json:"confidence,omitempty"`
	HasConfidence bool    `json:"hasConfidence,omitempty"`
	// Calibrated is true only when the numbers are calibrated probabilities
	// (true for Jev, false for an LLM emulator). Thresholds are applied only
	// to calibrated answers.
	Calibrated bool   `json:"calibrated"`
	NoneID     string `json:"noneId,omitempty"`
}

// NewAnswer builds an Answer with its scores sorted. The input slice is not
// modified.
func NewAnswer(questionID string, kind QuestionKind, scores []Score) Answer {
	sorted := slices.Clone(scores)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Probability != sorted[j].Probability {
			return sorted[i].Probability > sorted[j].Probability
		}
		return sorted[i].ID < sorted[j].ID
	})
	return Answer{QuestionID: questionID, Kind: kind, Scores: sorted}
}

// Probability returns the probability of candidate id (0, false if unscored).
func (a Answer) Probability(id string) (float64, bool) {
	for _, s := range a.Scores {
		if s.ID == id {
			return s.Probability, true
		}
	}
	return 0, false
}

// Usage is the billable size of one engine call, when the engine reports it.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// ScoreResult is an engine's answers to a ScoreRequest.
type ScoreResult struct {
	// Engine is the name of the engine that answered (the leaf engine, not a
	// combinator wrapping it).
	Engine string `json:"engine"`
	// Model is the engine's model id, as the engine reports it.
	Model   string            `json:"model,omitempty"`
	Usage   Usage             `json:"usage"`
	Answers map[string]Answer `json:"answers"`
	// Report is filled by combinators; see Report.
	Report *Report `json:"report,omitempty"`
}

// ScoredProvider scores candidates. It returns an error on failure; unlike a
// Provider it has no "abstain": a Choice that picks "none of these" is an
// answer (see OutcomeNone).
type ScoredProvider interface {
	Name() string
	Score(ctx context.Context, req ScoreRequest) (ScoreResult, error)
}

// TracedScorer is a ScoredProvider that also reports how it answered
// (engine, strategy, attempts, fallbacks), as the compose combinators do.
type TracedScorer interface {
	ScoredProvider
	ScoreTraced(ctx context.Context, req ScoreRequest) (ScoreResult, Report, error)
}

// ValidateScoreRequest checks req is well formed: at least one question, unique
// non-empty question ids, a known kind, at least one candidate, unique
// non-empty candidate ids, and a NoneID (if set) that names a candidate of a
// KindChoice question. The error matches ErrInvalidRequest (the caller is at
// fault, not the engine) and names problems by position (question 2, candidate
// 3), never by id: ids are the caller's content, and an error ends up in traces
// and logs.
func ValidateScoreRequest(req ScoreRequest) error {
	if len(req.Questions) == 0 {
		return fmt.Errorf("%w: no questions", ErrInvalidRequest)
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	seenQ := map[string]bool{}
	for i, q := range req.Questions {
		n := i + 1
		if q.ID == "" {
			add("question %d: id is required", n)
		} else if seenQ[q.ID] {
			add("question %d: duplicate question id", n)
		}
		seenQ[q.ID] = true
		if q.Kind != KindChoice && q.Kind != KindRelevance {
			add("question %d: unknown kind", n)
		}
		if len(q.Candidates) == 0 {
			add("question %d: no candidates", n)
		}
		seenC := map[string]bool{}
		for j, c := range q.Candidates {
			if c.ID == "" {
				add("question %d: candidate %d: id is required", n, j+1)
			} else if seenC[c.ID] {
				add("question %d: candidate %d: duplicate candidate id", n, j+1)
			}
			seenC[c.ID] = true
		}
		if q.NoneID != "" && (q.Kind != KindChoice || !seenC[q.NoneID]) {
			add("question %d: noneId must name a candidate of a choice question", n)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d problem(s): %s", ErrInvalidRequest, len(problems), strings.Join(problems, "; "))
}

// ValidateScoreResult checks res answers every question of req with only
// known candidates and finite probabilities in [0,1] (a KindChoice answer must
// also sum to about 1), and a confidence in [0,1]. A result that fails is
// treated by combinators as no answer from that engine.
func ValidateScoreResult(req ScoreRequest, res ScoreResult) error {
	var errs []error
	for _, q := range req.Questions {
		a, ok := res.Answers[q.ID]
		if !ok {
			errs = append(errs, fmt.Errorf("question %q: no answer", q.ID))
			continue
		}
		known := map[string]bool{}
		for _, c := range q.Candidates {
			known[c.ID] = true
		}
		sum := 0.0
		for _, s := range a.Scores {
			switch {
			case !known[s.ID]:
				errs = append(errs, fmt.Errorf("question %q: unknown candidate %q", q.ID, s.ID))
			case math.IsNaN(s.Probability) || s.Probability < 0 || s.Probability > 1:
				errs = append(errs, fmt.Errorf("question %q: probability of %q out of range", q.ID, s.ID))
			}
			sum += s.Probability
		}
		if q.Kind == KindChoice && len(a.Scores) > 0 && math.Abs(sum-1) > 0.05 {
			errs = append(errs, fmt.Errorf("question %q: choice probabilities sum to %.3f, not 1", q.ID, sum))
		}
		if a.HasConfidence && (math.IsNaN(a.Confidence) || a.Confidence < 0 || a.Confidence > 1) {
			errs = append(errs, fmt.Errorf("question %q: confidence out of range", q.ID))
		}
	}
	return errors.Join(errs...)
}
