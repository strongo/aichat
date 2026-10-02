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
// Selection.Actionable): a SelectionPolicy selected a calibrated answer
// (selected, several), or the caller's policy explicitly accepted an
// uncalibrated decision at a stated bar (accepted; SelectionPolicy.
// AcceptUncalibratedAt, off for DurablePolicy). An uncalibrated LLM emulator's
// self-reported confidence is otherwise a proposal, never a selection.
//
// A Chain with a Policy only returns ok=true for an answer that rule accepts; an
// uncertain, "none" or unscored answer falls through to the next provider. A
// chain built with KeepNonSelected can return ok=true for such an answer, and so
// can an engine called directly (compose.WithPolicy sets its Outcome): such a
// caller MUST check Decision.Actionable before acting on the decision. A chain
// without a Policy has only its MinConfidence floor, and an empty Outcome (no
// policy judged the answer) is then actionable.
package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	InteractionConfidence float64    `json:"interactionConfidence,omitempty"`
	Reference             *Reference `json:"reference,omitempty"`
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
	// calibrated scores.
	Calibrated bool `json:"calibrated,omitempty"`
	// Outcome is the policy's verdict, set by a Chain with a SelectionPolicy and
	// by an engine built with a policy. A caller that cannot rule out a chain
	// with KeepNonSelected, or that calls an engine directly, MUST act only on a
	// decision for which Actionable is true.
	Outcome Outcome `json:"outcome,omitempty"`
	// Model is the model id the engine reported for this decision ("" when it
	// reports none). Thresholds only hold for the model they were measured on.
	Model string `json:"model,omitempty"`
}

// Actionable reports whether a caller may act on d. The rule is one and
// explicit, the same as Selection.Actionable: a policy selected a calibrated
// answer (OutcomeSelected) or explicitly accepted an uncalibrated one at its
// stated bar (OutcomeAccepted, SelectionPolicy.AcceptUncalibratedAt). Uncertain,
// none and unscored decisions are not actionable. The one other actionable
// state is the empty Outcome, which means NO policy judged d: the answer of a
// Chain without a Policy, whose own MinConfidence is then the caller's explicit
// bar. Every engine built with a policy (compose.WithPolicy) and every Chain with
// a Policy sets an Outcome, so a decision is never actionable by omission.
func (d Decision) Actionable() bool {
	return d.Outcome == "" || d.Outcome.Actionable()
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
// must be in [0,1]; scopes, presentation, Reference.Kind and RequiredData
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
	if d.Module.Confidence < 0 || d.Module.Confidence > 1 || d.Intent.Confidence < 0 || d.Intent.Confidence > 1 {
		errs = append(errs, errors.New("confidence out of range"))
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
	// Calibrated, Outcome and Model echo the decision's flag, the policy's
	// verdict and the model id the answering engine reported.
	Calibrated bool    `json:"calibrated,omitempty"`
	Outcome    Outcome `json:"outcome,omitempty"`
	Model      string  `json:"model,omitempty"`
}

// Chain runs providers in order; the first valid, confident decision wins.
// A chain with no deciding provider is not an error: the product falls back
// to its main-LLM path (which classifies and answers in one inference).
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
}

// decisionTimeouter is the optional per-provider timeout override.
type decisionTimeouter interface {
	DecisionTimeout() time.Duration
}

// Decide runs the chain.
func (c Chain) Decide(ctx context.Context, req Request) (Decision, bool, Trace) {
	minConf := c.MinConfidence
	if minConf == 0 {
		minConf = 0.7
	}
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
		switch {
		case err != nil && errors.Is(err, ErrUnavailable):
			a.Outcome, a.Detail = AttemptUnavailable, err.Error()
		case err != nil && errors.Is(err, ErrAuth):
			a.Outcome, a.Detail = AttemptAuth, err.Error()
		case err != nil && errors.Is(err, ErrQuota):
			a.Outcome, a.Detail = AttemptQuota, err.Error()
		case err != nil && errors.Is(err, ErrMisconfigured):
			a.Outcome, a.Detail = AttemptMisconfigured, err.Error()
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
		if rep == nil {
			tr.Attempts = append(tr.Attempts, a)
		} else {
			tr.Attempts = append(tr.Attempts, MergeReport(*rep, a, ok && err == nil)...)
		}
		if decided {
			tr.DecidedBy = a.Provider
			tr.Engine = a.Provider
			if rep != nil {
				tr.Engine = rep.Engine
				tr.Strategy, tr.FallbackFired, tr.HedgeFired = rep.Strategy, rep.FallbackFired, rep.HedgeFired
			}
			tr.Calibrated, tr.Outcome, tr.Model = d.Calibrated, d.Outcome, d.Model
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

// judge classifies a provider's answer: invalid, rejected, or decided. With a
// SelectionPolicy set the policy alone owns the bar (EvaluateDecision): a
// calibrated answer is judged by its probabilities, an uncalibrated one is
// accepted only if the policy opts in (AcceptUncalibratedAt) and is unscored,
// not actionable, otherwise. An actionable verdict is "decided"; any other is
// recorded as "uncertain" and the chain falls through, unless KeepNonSelected
// makes it "decided" for the caller to read Decision.Outcome. Without a policy
// the MinConfidence floor is the bar; a provider that returned an explicit
// uncertain or none verdict of its own (an engine built with a policy) is still
// honoured, never acted on by omission.
func (c Chain) judge(d Decision, a Attempt, req Request, minConf float64) (Decision, Attempt) {
	if verr := Validate(d, req.Taxonomy); verr != nil {
		a.Outcome, a.Detail = AttemptInvalid, InvalidDetail(verr)
		return d, a
	}
	if c.Policy != nil {
		sel := c.Policy.EvaluateDecision(d)
		d.Outcome = sel.Outcome
		a.Detail = sel.Detail()
		a.Outcome = AttemptUncertain
		if sel.Actionable() || c.KeepNonSelected {
			a.Outcome = AttemptDecided
		}
		return d, a
	}
	if d.Outcome == OutcomeUncertain || d.Outcome == OutcomeNone {
		a.Detail = string(d.Outcome)
		a.Outcome = AttemptUncertain
		if c.KeepNonSelected {
			a.Outcome = AttemptDecided
		}
		return d, a
	}
	if lowConfidence(d, minConf) {
		a.Outcome = AttemptLowConfidence
		a.Detail = fmt.Sprintf("module=%.2f intent=%.2f", d.Module.Confidence, d.Intent.Confidence)
		return d, a
	}
	a.Outcome = AttemptDecided
	return d, a
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
