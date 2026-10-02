package decision

// Provenance says what stands behind a Decision's numbers, and so what a
// caller may do with it. It layers over Decision.Calibrated (which stays, for
// wire compatibility) and adds the third class that bool could not express.
type Provenance string

const (
	// ProvenanceCalibrated: the confidences and Scores are calibrated
	// probabilities from a real decision model (Decision.Calibrated is true). A
	// SelectionPolicy judges them by probability.
	ProvenanceCalibrated Provenance = "calibrated"
	// ProvenanceSelfReported: an LLM emulator's own confidence, which is a
	// proposal, not a probability. A policy acts on it only through its explicit
	// opt-in (SelectionPolicy.AcceptUncalibratedAt). The default for every
	// decision that is neither calibrated nor deterministic, so an engine that
	// says nothing is treated as the weakest class.
	ProvenanceSelfReported Provenance = "self_reported"
	// ProvenanceDeterministic: produced by exact, in-process logic with no model
	// in the loop (a rule table). It is always actionable (OutcomeDeterministic),
	// whatever the policy, and no AcceptUncalibratedAt bar applies to it: nothing
	// was estimated, so there is no confidence to threshold.
	ProvenanceDeterministic Provenance = "deterministic"
)

// Provenance returns the class of d. It is deterministic only when a provider
// declared it through Deterministic; calibrated when Calibrated is set; and
// self-reported otherwise.
func (d Decision) Provenance() Provenance {
	switch {
	case d.deterministic:
		return ProvenanceDeterministic
	case d.Calibrated:
		return ProvenanceCalibrated
	default:
		return ProvenanceSelfReported
	}
}

// Deterministic declares d the product of exact, deterministic logic (a rule
// match, a lookup) and returns it as such: provenance deterministic, outcome
// OutcomeDeterministic (actionable), and everything that would claim a model
// stood behind it cleared (Calibrated, Scores, Model).
//
// This is the ONLY way to make a decision deterministic, and it is deliberately
// out of reach of anything that merely produces data: the class lives in an
// unexported field, so it cannot be set by JSON (a cloud or remote response,
// persisted decisions) nor by an LLM's output, and a Decision copied through
// compose engines and breakers keeps it. A provider that calls Deterministic
// asserts, for each answer it returns, that no model produced it: calling it on
// an LLM's or a remote engine's answer defeats every selection policy and is a
// bug in that provider. ai/decision/rules.Provider calls it for every matched
// rule; a product's own deterministic provider (a lookup table, a command
// parser) may do the same.
//
// A deterministic decision is still validated (Validate) by the chain, and a
// Chain without a Policy still applies its MinConfidence floor to it, as the
// caller's explicit bar.
func Deterministic(d Decision) Decision {
	d.deterministic = true
	d.Calibrated = false
	d.Scores = nil
	d.Model = ""
	d.Outcome = OutcomeDeterministic
	return d
}
