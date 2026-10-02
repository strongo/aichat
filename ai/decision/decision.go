// Package decision defines the DecisionProvider contract and the Chain that
// runs providers in order until one decides.
//
// A decision answers, in a single inference, the conditional questions a
// product needs before doing any work: which module, which intent, what kind
// of interaction, what the user refers to, the minimum context scopes, the
// dynamic data to fetch, whether deterministic handling is possible, whether
// the main LLM is needed, and the suggested presentation.
//
// Decisions never carry entity IDs: providers identify a reference
// expression ("my dentist appointment tomorrow"); the product resolves it
// against real data.
//
// The schema is intentionally evolvable: new fields are additive, and
// consumers treat unknown module/intent/presentation values as "not decided".
//
// # What is sent to an engine
//
// Everything placed in Request (Text, Recent, State titles, Context, Taxonomy
// descriptions) or in a ScoreRequest (Text, Context, question instructions,
// candidate descriptions) is sent VERBATIM to the engine's operator: a hosted
// decision model or LLM, or a cloud decision endpoint. Send metadata (names,
// schemas, public descriptions), never row data, credentials or user
// identifiers; the product is responsible for what it puts there.
//
// # Acting on a chain's answer
//
// One rule decides whether an answer may be acted on (Decision.Actionable and
// Selection.Actionable): a positive verdict was stamped on it by this package's
// judges. A decision never becomes actionable by omission or by claim: an engine or
// provider used directly returns it unjudged, which is NOT actionable until a
// Chain, or an engine built with a policy (compose.WithPolicy), judged it; the
// zero Decision is not actionable; and the exported Outcome field, which is kept on
// the wire for traces and telemetry, is informational only: what a remote engine,
// a stored decision or a provider writes there is never believed.
//
// The positive verdicts are: a SelectionPolicy selected a calibrated answer
// (selected, several); the policy explicitly accepted an uncalibrated LLM
// self-report at a stated bar (accepted; SelectionPolicy.AcceptUncalibratedAt, off
// for DurablePolicy); the decision is deterministic (deterministic: it came from
// exact logic such as a rule table, declared with Deterministic, and no policy
// threshold applies to it); or a Chain without a Policy accepted it at its
// MinConfidence floor (floor). An LLM emulator's self-reported confidence is
// otherwise a proposal, never a selection.
//
// A side-effectful interaction (SideEffectful: confirmation, rejection,
// correction, cancellation, undo) is held to one more bar, with or without a
// policy: its own InteractionConfidence must be above 0 and at least
// DurableMinConfidence, and a CALIBRATED claim about it must be backed by
// Decision.InteractionScores (a remote engine's calibrated flag is only a claim);
// an unbacked one counts as a self-report, which a policy accepts only with
// SelectionPolicy.AcceptUncalibratedSideEffects. Deterministic decisions are exact
// and exempt.
//
// A Chain with a Policy only returns ok=true for an answer that rule accepts; an
// uncertain, "none", unscored or invalid answer falls through to the next
// provider. A chain built with KeepNonSelected can return ok=true for such an
// answer (but never an invalid one), and so can an engine called directly
// (compose.WithPolicy stamps its verdict): such a caller MUST check
// Decision.Actionable before acting on the decision.
//
// # Stored and replayed decisions
//
// A Decision that was marshalled and read back (a database row, a trace, a cloud
// response) is NOT actionable and has lost its provenance class: the verdict and
// the deterministic class live in unexported fields no data can set, so a rule
// decision replayed from storage reads self_reported. A product that replays must
// re-judge it through a chain or policy (Chain.Rejudge, SelectionPolicy.JudgeDecision),
// which treats it as the ordinary decision it now is, or re-run its rules.
//
// # Stopped chains
//
// A Chain stops at an exhausted allowance (ErrQuota), a spent budget (ErrBudget,
// see compose.NewBudget) or a misconfigured engine (ErrMisconfigured) instead of
// handing the call to the next, possibly paid, provider (Chain.StopOnQuota,
// Chain.StopOnMisconfigured). A stopped chain returns ok=false, exactly like a
// chain nobody decided in, so a product MUST check Trace.StoppedBy (and use
// Trace.Err) before treating ok=false as "use the paid main-LLM path": the stop
// exists to keep that path from being billed. Place deterministic providers (rules)
// BEFORE the engines: a stop skips every provider after the one that stopped.
package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/session"
)

// Interaction classifies the user's turn.
type Interaction string

const (
	InteractionCommand      Interaction = "command"
	InteractionQuestion     Interaction = "question"
	InteractionConfirmation Interaction = "confirmation"
	InteractionRejection    Interaction = "rejection"
	InteractionCorrection   Interaction = "correction"
	InteractionContinuation Interaction = "continuation"
	InteractionCancellation Interaction = "cancellation"
	InteractionUndo         Interaction = "undo"
	InteractionChat         Interaction = "chat" // general conversation
)

// Scored is a value with the provider's confidence in [0,1].
type Scored struct {
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
}

// Reference is what the user refers to, as an expression to resolve.
type Reference struct {
	Kind       string `json:"kind"`                 // entity type, e.g. "happening"
	Expression string `json:"expression,omitempty"` // "my dentist appointment tomorrow"
	// Pronoun is true for "it"/"that"/"this one": resolve from session state.
	Pronoun bool `json:"pronoun,omitempty"`
}

// Decision is a provider's answer.
type Decision struct {
	Module      Scored      `json:"module"`
	Intent      Scored      `json:"intent"`
	Interaction Interaction `json:"interaction"`
	// InteractionConfidence is the engine's own confidence in Interaction, in
	// [0,1], when the engine reports one (0 otherwise). It is additive:
	// Interaction is only ever set from an answer the engine's selection policy
	// selected, so a caller need not threshold it, but it may apply a stricter
	// bar of its own to an interaction that acts on a pending action.
	InteractionConfidence float64 `json:"interactionConfidence,omitempty"`
	// InteractionScores are a calibrated engine's probabilities for the options of
	// the Choice that picked Interaction, keyed by Interaction value. It is the
	// evidence a calibrated claim about a SIDE-EFFECTFUL interaction needs (see
	// SideEffectful): without it, or when it contradicts Interaction, the claim is
	// only as strong as a self-report. Additive; empty when the engine has none.
	InteractionScores map[string]float64 `json:"interactionScores,omitempty"`
	Reference         *Reference         `json:"reference,omitempty"`
	// RequiredScopes is the MINIMUM context needed. It does not mean other
	// cached scopes must be dropped; see package ctxmgr.
	RequiredScopes []string `json:"requiredScopes,omitempty"`
	// RequiredData names dynamic data to fetch, e.g. "relevant_happenings".
	RequiredData []string `json:"requiredData,omitempty"`
	// Slots are extracted arguments ("when": "Friday 16:00", "title": ...).
	Slots                      map[string]string `json:"slots,omitempty"`
	CanHandleDeterministically bool              `json:"canHandleDeterministically"`
	NeedsLLM                   bool              `json:"needsLLM"`
	Presentation               string            `json:"presentation,omitempty"` // e.g. "day_calendar"

	// Scores are the engine's probabilities for the options of the Choice that
	// picked Intent, keyed by option id (for a taxonomy of several modules the
	// option id is "module/intent"). Empty when the engine provides none.
	Scores map[string]float64 `json:"scores,omitempty"`
	// Calibrated is true only when Scores and the confidences are calibrated
	// probabilities (a real decision model), false for an LLM emulator's
	// self-reported confidence. A SelectionPolicy is applied only to
	// calibrated scores. Provenance layers a third class (deterministic) over
	// this flag.
	Calibrated bool `json:"calibrated,omitempty"`
	// Outcome is the verdict as a record: a Chain (with or without a
	// SelectionPolicy), SelectionPolicy.JudgeDecision and an engine built with a
	// policy set it, and it is kept on the wire so traces and telemetry can show it.
	// It is INFORMATIONAL: what a remote engine, a stored decision or a provider
	// writes here is never trusted. Whether a caller may act is decided by the
	// unexported verdict only those judges stamp, which Actionable reads, so a
	// decision that came off the wire or out of storage is NOT actionable whatever
	// its Outcome says (see Rejudge). A caller that cannot rule out a chain with
	// KeepNonSelected, or that calls an engine or provider directly, MUST act only
	// on a decision for which Actionable is true.
	Outcome Outcome `json:"outcome,omitempty"`
	// Model is the model id the engine reported for this decision ("" when it
	// reports none). Thresholds only hold for the model they were measured on.
	Model string `json:"model,omitempty"`

	// deterministic marks a decision produced by exact logic (see Deterministic).
	// Unexported on purpose: no JSON from a remote engine, no LLM output and no
	// stored decision can set it.
	deterministic bool
	// verdict is the judged state: the positive or refusing Outcome stamped by this
	// package's judges (Chain, SelectionPolicy.JudgeDecision, Deterministic) and by
	// nothing else. Unexported for the same reason as deterministic: no JSON and no
	// provider can set it, so no decision becomes actionable by claiming an Outcome.
	verdict Outcome
}

// stamped returns d judged as o: the unexported verdict and the exported record.
func (d Decision) stamped(o Outcome) Decision {
	d.verdict, d.Outcome = o, o
	return d
}

// Actionable reports whether a caller may act on d. The rule is one and
// explicit, the same as Selection.Actionable: this package's judges stamped a
// positive verdict on d (selected, several, accepted, deterministic or floor; see
// Outcome.Actionable). The exported Outcome is not consulted: it is informational
// and can be written by JSON or by a provider. An unjudged decision is not
// actionable: a provider or engine used directly returns one, the zero Decision
// is one, and so is every decision that was marshalled and read back (a stored or
// remote decision has lost its verdict and its provenance class; re-judge it with
// Chain.Rejudge). Chain (with or without a Policy) and every engine built with a
// policy stamp the verdict.
func (d Decision) Actionable() bool {
	return d.verdict.Actionable()
}

// ModuleSpec declares a product module and its intents.
type ModuleSpec struct {
	Name    string   `json:"name"`
	Intents []string `json:"intents"`
	// Scopes this module's intents may require (defaults to [Name]).
	Scopes []string `json:"scopes,omitempty"`
}

// Taxonomy is the product's decision vocabulary, sent with every request so a
// shared decision service stays product-neutral.
type Taxonomy struct {
	Modules       []ModuleSpec `json:"modules"`
	Presentations []string     `json:"presentations,omitempty"`
	DataKinds     []string     `json:"dataKinds,omitempty"`
	EntityTypes   []string     `json:"entityTypes,omitempty"`
	// Descriptions optionally explain taxonomy entries to the engine, keyed by
	// entry name: a module name, "module/intent", a presentation, a data kind
	// or an entity type. A bare name often scores poorly; one line of public
	// description helps. Additive and optional.
	Descriptions map[string]string `json:"descriptions,omitempty"`
}

// Request is the input to Decide.
type Request struct {
	Product       string            `json:"product"`
	InteractionID string            `json:"interactionId,omitempty"`
	ClientContext *ai.ClientContext `json:"clientContext,omitempty"`
	Text          string            `json:"text"`
	Taxonomy      Taxonomy          `json:"taxonomy"`
	State         session.State     `json:"state"` // entity refs only, no rendered data
	// Recent is a short tail of the transcript for continuations.
	Recent []string `json:"recent,omitempty"`
	// Context is optional JSON-able context for the engine: names and public
	// metadata only, never row data, credentials or user identifiers. It is
	// additive and optional.
	Context map[string]any `json:"context,omitempty"`
	Now     time.Time      `json:"now"`
	TZ      string         `json:"tz,omitempty"`
}

// Provider decides or abstains. It returns (d, true, nil) when it decided,
// (_, false, nil) to abstain, and a non-nil error on failure. Callers treat
// an error exactly like abstention (after recording it).
type Provider interface {
	Name() string
	Decide(ctx context.Context, req Request) (Decision, bool, error)
}

// moduleOptionalInteractions is the set of Interaction values that make
// sense with no Module/Intent at all -- "Yes", "No", "Cancel", "Undo that"
// answer the PREVIOUS turn's pending action rather than naming a module, and
// plain chat ("How's it going?", "thanks!") isn't routed to any product
// module at all -- so none of them has anything module-shaped to report.
// See REQ: rules-friendly-validation.
var moduleOptionalInteractions = map[Interaction]bool{
	InteractionConfirmation: true,
	InteractionRejection:    true,
	InteractionCancellation: true,
	InteractionUndo:         true,
	InteractionChat:         true,
}

// sideEffectful lists the interactions that act on a pending or previous
// action: when wrong they are a side effect (confirming, cancelling or undoing
// something the user never meant).
var sideEffectful = map[Interaction]bool{
	InteractionConfirmation: true,
	InteractionRejection:    true,
	InteractionCorrection:   true,
	InteractionCancellation: true,
	InteractionUndo:         true,
}

// SideEffectful reports whether i acts on a pending or previous action
// (confirmation, rejection, correction, cancellation, undo), so a wrong answer
// is a side effect rather than a wasted lookup. A policy never accepts such an
// interaction from an uncalibrated engine without an explicit opt-in
// (SelectionPolicy.AcceptUncalibratedSideEffects), and never at confidence 0.
func SideEffectful(i Interaction) bool { return sideEffectful[i] }

// knownInteractions is the full Interaction enum.
var knownInteractions = map[Interaction]bool{
	InteractionCommand:      true,
	InteractionQuestion:     true,
	InteractionConfirmation: true,
	InteractionRejection:    true,
	InteractionCorrection:   true,
	InteractionContinuation: true,
	InteractionCancellation: true,
	InteractionUndo:         true,
	InteractionChat:         true,
}

// Validate checks d against the taxonomy: Interaction must be a known, non-
// empty value; module and intent must be declared UNLESS Interaction is one
// of the module-optional kinds (confirmation/rejection/cancellation/undo)
// and Module is empty, in which case both checks are skipped; confidences
// (module, intent, interaction) must be in [0,1]; scopes, presentation, Reference.Kind and RequiredData
// must be known to the taxonomy when the taxonomy declares a non-empty list
// for that dimension. A decision that fails validation is treated as an
// abstention.
func Validate(d Decision, t Taxonomy) error {
	var errs []error
	if d.Interaction == "" {
		errs = append(errs, errors.New("interaction is required"))
	} else if !knownInteractions[d.Interaction] {
		errs = append(errs, fmt.Errorf("unknown interaction %q", d.Interaction))
	}
	confidences := []float64{d.Module.Confidence, d.Intent.Confidence, d.InteractionConfidence}
	for _, p := range d.InteractionScores {
		confidences = append(confidences, p)
	}
	for _, c := range confidences {
		if math.IsNaN(c) || c < 0 || c > 1 {
			errs = append(errs, errors.New("confidence out of range"))
			break
		}
	}
	moduleOptional := d.Module.Value == "" && moduleOptionalInteractions[d.Interaction]
	if !moduleOptional {
		i := slices.IndexFunc(t.Modules, func(m ModuleSpec) bool { return m.Name == d.Module.Value })
		if i < 0 {
			errs = append(errs, fmt.Errorf("unknown module %q", d.Module.Value))
		} else if d.Intent.Value != "" && !slices.Contains(t.Modules[i].Intents, d.Intent.Value) {
			errs = append(errs, fmt.Errorf("unknown intent %q for module %q", d.Intent.Value, d.Module.Value))
		}
	}
	if d.Presentation != "" && len(t.Presentations) > 0 && !slices.Contains(t.Presentations, d.Presentation) {
		errs = append(errs, fmt.Errorf("unknown presentation %q", d.Presentation))
	}
	if d.Reference != nil && d.Reference.Kind != "" && len(t.EntityTypes) > 0 && !slices.Contains(t.EntityTypes, d.Reference.Kind) {
		errs = append(errs, fmt.Errorf("unknown reference kind %q", d.Reference.Kind))
	}
	if len(t.DataKinds) > 0 {
		for _, dk := range d.RequiredData {
			if !slices.Contains(t.DataKinds, dk) {
				errs = append(errs, fmt.Errorf("unknown required data kind %q", dk))
			}
		}
	}
	known := map[string]bool{}
	for _, m := range t.Modules {
		known[m.Name] = true
		for _, s := range m.Scopes {
			known[s] = true
		}
	}
	for _, s := range d.RequiredScopes {
		if !known[s] {
			errs = append(errs, fmt.Errorf("unknown scope %q", s))
		}
	}
	return errors.Join(errs...)
}

// Attempt records one provider's outcome for diagnostics.
type Attempt struct {
	Provider string `json:"provider"`
	Outcome  string `json:"outcome"` // see the Attempt* constants
	Detail   string `json:"detail,omitempty"`
	// Latency is the wall-clock time the attempt took. On the wire it is the
	// integer "latencyMs" (milliseconds, rounded down); a reader that still sees
	// the legacy "latency" field (nanoseconds, the encoding of a Go duration) uses
	// it only when "latencyMs" is absent, and the encoder still writes it, so
	// readers of either vintage work. See Attempt.MarshalJSON.
	Latency time.Duration `json:"-"`
	// Role is the engine's part in a combinator: "primary", "backup" or
	// "racer"; empty for a plain chain provider.
	Role string `json:"role,omitempty"`
	// Usage is what THIS attempt consumed, when its engine reported it (a scored
	// call answered by the engine). A hedged or fallen-back call has several
	// attempts, each possibly billed, so metering per engine needs the per-attempt
	// figure, not only the answering engine's ScoreResult.Usage. An attempt that
	// reported none (a cancelled loser, a failure) carries nil, not zero: its true
	// cost is unknown.
	Usage *Usage `json:"usage,omitempty"`
}

// attemptWire is the JSON form of an Attempt.
type attemptWire struct {
	Provider  string        `json:"provider"`
	Outcome   string        `json:"outcome"`
	Detail    string        `json:"detail,omitempty"`
	Latency   time.Duration `json:"latency"` // legacy, nanoseconds
	LatencyMs *int64        `json:"latencyMs,omitempty"`
	Role      string        `json:"role,omitempty"`
	Usage     *Usage        `json:"usage,omitempty"`
}

// MarshalJSON writes Latency as the integer "latencyMs" and, for readers that
// predate it, as the legacy "latency" in nanoseconds.
func (a Attempt) MarshalJSON() ([]byte, error) {
	ms := a.Latency.Milliseconds()
	return json.Marshal(attemptWire{a.Provider, a.Outcome, a.Detail, a.Latency, &ms, a.Role, a.Usage})
}

// UnmarshalJSON reads "latencyMs" when present, else the legacy "latency".
func (a *Attempt) UnmarshalJSON(b []byte) error {
	var w attemptWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*a = Attempt{Provider: w.Provider, Outcome: w.Outcome, Detail: w.Detail, Latency: w.Latency, Role: w.Role, Usage: w.Usage}
	if w.LatencyMs != nil {
		a.Latency = time.Duration(*w.LatencyMs) * time.Millisecond
	}
	return nil
}

// Trace is the chain's diagnostic record.
type Trace struct {
	DecidedBy string    `json:"decidedBy,omitempty"` // "" when every provider abstained
	Attempts  []Attempt `json:"attempts"`
	// Engine is the leaf engine that produced the decision (differs from
	// DecidedBy when a combinator wrapped it).
	Engine string `json:"engine,omitempty"`
	// Strategy, FallbackFired and HedgeFired describe the combinator that
	// answered, if any (see Report).
	Strategy      string `json:"strategy,omitempty"`
	FallbackFired bool   `json:"fallbackFired,omitempty"`
	HedgeFired    bool   `json:"hedgeFired,omitempty"`
	// Calibrated, Provenance, Outcome and Model echo the decision's flag, its
	// class (calibrated, self_reported or deterministic), the verdict and the
	// model id the answering engine reported.
	Calibrated bool       `json:"calibrated,omitempty"`
	Provenance Provenance `json:"provenance,omitempty"`
	Outcome    Outcome    `json:"outcome,omitempty"`
	Model      string     `json:"model,omitempty"`
	// StoppedBy is set when the chain stopped early instead of trying its next
	// provider: AttemptQuota (an exhausted allowance) or AttemptMisconfigured (an
	// endpoint a person has to fix). The decision is then ok=false, like any chain
	// that decided nothing, but the product MUST NOT read it as "nobody decided,
	// use the paid main-LLM path" without choosing to: Err returns the error.
	StoppedBy string `json:"stoppedBy,omitempty"`
}

// Err returns the error a chain that stopped early (Trace.StoppedBy) stands for:
// it matches (errors.Is) ErrQuota, ErrBudget or ErrMisconfigured and names the
// provider that said so. It is nil for a trace that was not stopped. The message is
// the provider's own error text, as the attempt's detail has it, so the condition
// is stated once.
func (t Trace) Err() error {
	var sentinel error
	switch t.StoppedBy {
	case AttemptQuota:
		sentinel = ErrQuota
	case AttemptBudget:
		sentinel = ErrBudget
	case AttemptMisconfigured:
		sentinel = ErrMisconfigured
	default:
		return nil
	}
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		if a := t.Attempts[i]; a.Outcome == t.StoppedBy {
			return &stoppedError{msg: fmt.Sprintf("decision: chain stopped at %s (%s): %s", a.Provider, t.StoppedBy, a.Detail), kind: sentinel}
		}
	}
	return &stoppedError{msg: fmt.Sprintf("decision: chain stopped (%s): %s", t.StoppedBy, sentinel), kind: sentinel}
}

// stoppedError is Trace.Err's value: a fixed message that matches its sentinel.
type stoppedError struct {
	msg  string
	kind error
}

func (e *stoppedError) Error() string { return e.msg }
func (e *stoppedError) Unwrap() error { return e.kind }

// StopPolicy says whether a Chain stops when a provider reports a condition it
// must not paper over. The zero value STOPS: stopping is the default, and falling
// through to the next provider is the deliberate, named opt-out.
type StopPolicy uint8

const (
	// StopChain (the zero value) stops the chain: no later provider is called, the
	// decision is ok=false and Trace.StoppedBy / Trace.Err say why.
	StopChain StopPolicy = iota
	// FallThrough lets the chain go on to the next provider, as a plain failure
	// does. Choosing it for a quota or a budget means a paid provider behind it can
	// take over the traffic.
	FallThrough
)

func (s StopPolicy) stops() bool { return s != FallThrough }

// String is "stop" or "fall_through" (and "StopPolicy(n)" for anything else).
func (s StopPolicy) String() string {
	switch s {
	case StopChain:
		return "stop"
	case FallThrough:
		return "fall_through"
	}
	return fmt.Sprintf("StopPolicy(%d)", uint8(s))
}

// MarshalText writes String, so a policy marshals to JSON as a string.
func (s StopPolicy) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads "stop" or "fall_through"; anything else is an error.
func (s *StopPolicy) UnmarshalText(b []byte) error {
	switch string(b) {
	case "stop":
		*s = StopChain
	case "fall_through":
		*s = FallThrough
	default:
		return fmt.Errorf("decision: unknown stop policy %q (want stop or fall_through)", b)
	}
	return nil
}

// Chain runs providers in order; the first valid, confident decision wins.
// A chain with no deciding provider is not an error: the product falls back
// to its main-LLM path (which classifies and answers in one inference), unless
// the chain stopped (Trace.StoppedBy; see the package doc). List deterministic
// providers (ai/decision/rules) first: they are free and exact, and a stop at an
// engine would otherwise skip them.
type Chain struct {
	Providers []Provider
	// MinConfidence is the minimum Module and Intent confidence to accept a
	// decision. Intent confidence is ignored when Intent is "", and Module
	// confidence is ignored entirely for a module-optional decision (see
	// Validate). Three cases:
	//   - MinConfidence == 0 (the zero value): default 0.7.
	//   - MinConfidence > 0: used as given.
	//   - MinConfidence < 0: "accept any" -- no confidence floor at all
	//     (a valid Scored.Confidence is always >= 0, so nothing is ever
	//     rejected on confidence grounds). Use this for a chain whose
	//     providers are deterministic and don't produce calibrated
	//     probabilities worth thresholding.
	MinConfidence float64
	// Timeout bounds each provider call (default 1500ms) UNLESS the
	// provider itself implements `interface{ DecisionTimeout() time.Duration }`,
	// in which case that provider's own value is used instead -- a remote
	// decision call reasonably wants more time than a local one.
	Timeout time.Duration
	// Policy, when non-nil, replaces MinConfidence for answers that carry
	// calibrated Scores: the policy alone decides selected / uncertain / none.
	// Only a selected answer stops the chain; an uncertain or "none" answer is
	// recorded in the trace (outcomes "uncertain" and "none") and the chain
	// falls through to the next provider, exactly as a low-confidence answer
	// does without a policy. A nil Policy keeps the legacy MinConfidence
	// behaviour exactly.
	Policy *SelectionPolicy
	// KeepNonSelected makes a non-actionable answer (uncertain, "none",
	// unscored) stop the chain and be returned with ok=true and Decision.Outcome
	// set, so a caller can show "not sure" instead of escalating. It is off by
	// default because ok=true is then not enough to act on: check
	// Decision.Actionable.
	KeepNonSelected bool
	// StopOnQuota stops the chain, instead of trying its next provider, when a
	// provider reports an exhausted allowance (ErrQuota, attempt outcome "quota") or
	// an exhausted spending cap (ErrBudget, outcome "budget"; see compose.NewBudget).
	// The zero value STOPS: an exhausted allowance must be loud and must not
	// silently bill a paid backup that follows it in the chain; the product reads
	// Trace.StoppedBy and Trace.Err. Opt out with FallThrough, naming the decision.
	// An engine that was given compose.OnQuota has already chosen to fail over, and
	// reports no quota error when its backup answers.
	//
	// A stopped chain returns ok=false, exactly like a chain nobody decided in: a
	// product MUST check Trace.StoppedBy before treating ok=false as "use the paid
	// main-LLM path", because the stop is there to keep that path from being billed.
	//
	// A stop also skips every provider after the one that stopped, deterministic
	// ones included: put rules BEFORE the engines (aiconfig does).
	StopOnQuota StopPolicy
	// StopOnMisconfigured is StopOnQuota for ErrMisconfigured (an unknown product,
	// a base URL that does not speak the protocol): a person has to fix it, so it is
	// never absorbed by the next provider. The zero value stops.
	StopOnMisconfigured StopPolicy
}

// minConfidence is MinConfidence with its zero value read as the 0.7 default.
func (c Chain) minConfidence() float64 {
	if c.MinConfidence == 0 {
		return 0.7
	}
	return c.MinConfidence
}

// decisionTimeouter is the optional per-provider timeout override.
type decisionTimeouter interface {
	DecisionTimeout() time.Duration
}

// Decide runs the chain.
func (c Chain) Decide(ctx context.Context, req Request) (Decision, bool, Trace) {
	minConf := c.minConfidence()
	defaultTimeout := c.Timeout
	if defaultTimeout == 0 {
		defaultTimeout = 1500 * time.Millisecond
	}
	var tr Trace
	for _, p := range c.Providers {
		timeout := defaultTimeout
		if dt, ok := p.(decisionTimeouter); ok {
			if v := dt.DecisionTimeout(); v > 0 {
				timeout = v
			}
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		var (
			d   Decision
			ok  bool
			err error
			rep *Report
		)
		if tp, traced := p.(TracedProvider); traced {
			var r Report
			d, ok, r, err = tp.DecideTraced(pctx, req)
			rep = &r
		} else {
			d, ok, err = p.Decide(pctx, req)
		}
		a := Attempt{Provider: p.Name(), Latency: time.Since(start)}
		// errors.Is (not ==) so a provider that wraps ctx.Err() (e.g.
		// fmt.Errorf("...: %w", ctx.Err())) still classifies as a timeout
		// rather than a generic error.
		timedOut := err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(pctx.Err(), context.DeadlineExceeded))
		cancel()
		// A condition that must stop the chain is classified before the ordinary
		// failures: an engine that joins several legs' errors (a Fallback whose
		// primary was unavailable and whose backup was out of budget) must not
		// have the loud one hidden behind the quiet one.
		switch {
		case err != nil && errors.Is(err, ErrQuota):
			a.Outcome, a.Detail = AttemptQuota, err.Error()
		case err != nil && errors.Is(err, ErrBudget):
			a.Outcome, a.Detail = AttemptBudget, err.Error()
		case err != nil && errors.Is(err, ErrMisconfigured):
			a.Outcome, a.Detail = AttemptMisconfigured, err.Error()
		case err != nil && errors.Is(err, ErrUnavailable):
			a.Outcome, a.Detail = AttemptUnavailable, err.Error()
		case err != nil && errors.Is(err, ErrAuth):
			a.Outcome, a.Detail = AttemptAuth, err.Error()
		case err != nil && errors.Is(err, ErrInvalidRequest):
			a.Outcome, a.Detail = AttemptRejected, err.Error()
		case err != nil && timedOut:
			a.Outcome, a.Detail = AttemptTimeout, err.Error()
		case err != nil:
			a.Outcome, a.Detail = AttemptError, err.Error()
		case !ok:
			a.Outcome = AttemptAbstained
		default:
			d, a = c.judge(d, a, req, minConf)
		}
		decided := a.Outcome == AttemptDecided
		stop := ((a.Outcome == AttemptQuota || a.Outcome == AttemptBudget) && c.StopOnQuota.stops()) || (a.Outcome == AttemptMisconfigured && c.StopOnMisconfigured.stops())
		if rep == nil {
			tr.Attempts = append(tr.Attempts, a)
		} else {
			tr.Attempts = append(tr.Attempts, MergeReport(*rep, a, ok && err == nil)...)
		}
		if stop {
			tr.StoppedBy = a.Outcome
			return Decision{}, false, tr
		}
		if decided {
			tr.DecidedBy = a.Provider
			tr.Engine = a.Provider
			if rep != nil {
				if rep.Engine != "" {
					tr.Engine = rep.Engine
				}
				tr.Strategy, tr.FallbackFired, tr.HedgeFired = rep.Strategy, rep.FallbackFired, rep.HedgeFired
			}
			tr.Calibrated, tr.Provenance, tr.Outcome, tr.Model = d.Calibrated, d.Provenance(), d.Outcome, d.Model
			if rep != nil && rep.Model != "" {
				tr.Model = rep.Model
			}
			return d, true, tr
		}
		if ctx.Err() != nil {
			break
		}
	}
	return Decision{}, false, tr
}

// judge classifies a provider's answer: invalid, rejected, or decided, and stamps
// the verdict. With a SelectionPolicy set the policy alone owns the bar
// (EvaluateDecision): a calibrated answer is judged by its probabilities, a
// deterministic one (see Deterministic) is always accepted, an uncalibrated one is
// accepted only if the policy opts in (AcceptUncalibratedAt) and is unscored, not
// actionable, otherwise; an answer that contradicts its own scores is invalid. An
// actionable verdict is "decided"; an invalid one falls through whatever
// KeepNonSelected says; any other is recorded as "uncertain" and the chain falls
// through, unless KeepNonSelected makes it "decided" for the caller to read
// Decision.Actionable. Without a policy the MinConfidence floor is the bar and a
// passing answer is ALWAYS stamped OutcomeFloor (OutcomeDeterministic for a
// deterministic one), whatever Outcome the provider wrote: only a verdict stamped
// by this package's judges counts, so a provider cannot claim "selected" or
// "deterministic" for itself. A non-actionable verdict stamped by an engine built
// with a policy (uncertain, none, unscored) is still honoured.
func (c Chain) judge(d Decision, a Attempt, req Request, minConf float64) (Decision, Attempt) {
	engineVerdict := d.verdict
	d.verdict, d.Outcome = "", "" // a claimed or stale verdict never survives; the judge stamps its own
	if verr := Validate(d, req.Taxonomy); verr != nil {
		a.Outcome, a.Detail = AttemptInvalid, InvalidDetail(verr)
		return d.stamped(OutcomeInvalid), a
	}
	if c.Policy != nil {
		var sel Selection
		d, sel = c.Policy.JudgeDecision(d)
		a.Detail = sel.Detail()
		switch {
		case sel.Outcome == OutcomeInvalid:
			a.Outcome = AttemptInvalid
		case sel.Actionable() || c.KeepNonSelected:
			a.Outcome = AttemptDecided
		default:
			a.Outcome = AttemptUncertain
		}
		return d, a
	}
	if engineVerdict != "" && !engineVerdict.Actionable() {
		a.Detail = string(engineVerdict)
		a.Outcome = AttemptUncertain
		if c.KeepNonSelected {
			a.Outcome = AttemptDecided
		}
		return d.stamped(engineVerdict), a
	}
	if lowConfidence(d, minConf) {
		a.Outcome = AttemptLowConfidence
		a.Detail = fmt.Sprintf("module=%.2f intent=%.2f", d.Module.Confidence, d.Intent.Confidence)
		return d, a
	}
	if why := sideEffectUnsupported(d, minConf); why != "" {
		a.Outcome = AttemptLowConfidence
		a.Detail = why
		return d, a
	}
	if d.deterministic {
		d = d.stamped(OutcomeDeterministic)
	} else {
		d = d.stamped(OutcomeFloor)
	}
	a.Outcome = AttemptDecided
	return d, a
}

// Rejudge judges d the way this chain judges a provider's answer (its Policy, or
// its MinConfidence floor, and the side-effect gate), and reports whether the
// result is actionable. It is how a product acts on a decision it stored or
// received: a decision read back from JSON (a database row, a cloud response)
// is NOT actionable and has lost its provenance class (deterministic included),
// because the verdict and the class live in unexported fields no data can set.
// Rejudge discards whatever Outcome the decision carries and judges it afresh, and
// can never make it deterministic: a product that must replay a rule decision as
// deterministic re-runs its rules (ai/decision/rules) instead. The taxonomy
// validates it, as Request.Taxonomy does for a provider's answer. A decision that
// is not actionable is returned stamped with its own outcome (OutcomeInvalid for
// one that fails validation) and ok=false, whatever Chain.KeepNonSelected says.
func (c Chain) Rejudge(d Decision, taxonomy Taxonomy) (Decision, bool) {
	d.deterministic = false // a decision is deterministic only in the process that matched the rule
	d, _ = c.judge(d, Attempt{}, Request{Taxonomy: taxonomy}, c.minConfidence())
	return d, d.Actionable()
}

// sideEffectUnsupported closes the hole a side-effectful interaction ("yes",
// "cancel", "undo that") leaves in a policy-less chain: the module-optional
// exemption of lowConfidence gives it no module or intent confidence to fail the
// floor, so it would pass at confidence 0, and a remote engine's calibrated flag
// is only a claim. Such a decision needs its own InteractionConfidence, above 0 and
// at the larger of the floor and DurableMinConfidence, with or without a policy
// (SelectionPolicy.EvaluateDecision applies the same gate); a calibrated claim
// additionally needs InteractionScores that back it, else it counts as a
// self-report (the bar above is then the whole gate). Deterministic decisions are
// exact and unaffected. It returns why the decision is refused, "" when it is not.
func sideEffectUnsupported(d Decision, minConf float64) string {
	if d.deterministic || !SideEffectful(d.Interaction) {
		return ""
	}
	pol := DurablePolicy()
	pol.MinConfidence = max(pol.MinConfidence, minConf)
	if reason := pol.interactionRefusal(d); reason != "" {
		return fmt.Sprintf("interaction=%.2f (%s)", d.InteractionConfidence, reason)
	}
	return ""
}

// MergeReport returns the attempts to record for a TracedProvider: the engines
// it ran, with the answering engine's outcome overridden by the caller's own
// judgement (invalid, low_confidence, a policy verdict, carried by judged) when
// the provider returned an answer. When the provider reported no attempts,
// judged itself is recorded instead.
func MergeReport(rep Report, judged Attempt, answered bool) []Attempt {
	if len(rep.Attempts) == 0 {
		return []Attempt{judged}
	}
	out := slices.Clone(rep.Attempts)
	if answered {
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Provider == rep.Engine && out[i].Outcome == AttemptDecided {
				out[i].Outcome, out[i].Detail = judged.Outcome, judged.Detail
				if out[i].Usage == nil {
					out[i].Usage = judged.Usage
				}
				break
			}
		}
	}
	return out
}

// lowConfidence reports whether d fails the minConf floor. A module-optional
// decision (see Validate) with an empty Module is exempt from the Module
// confidence check -- there is no module confidence to have an opinion
// about. minConf < 0 means "accept any": nothing is ever low-confidence.
func lowConfidence(d Decision, minConf float64) bool {
	if minConf < 0 {
		return false
	}
	moduleOptional := d.Module.Value == "" && moduleOptionalInteractions[d.Interaction]
	if !moduleOptional && d.Module.Confidence < minConf {
		return true
	}
	if d.Intent.Value != "" && d.Intent.Confidence < minConf {
		return true
	}
	return false
}
