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
package decision

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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
	Reference   *Reference  `json:"reference,omitempty"`
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
}

// ModuleSpec declares a product module and its intents.
type ModuleSpec struct {
	Name    string   `json:"name"`
	Intents []string `json:"intents"`
	// Scopes this module's intents may require (defaults to [Name]).
	Scopes []string `json:"scopes,omitempty"`
}

// Taxonomy is the product's decision vocabulary, sent with every request so a
// shared decision service (Jev) stays product-neutral.
type Taxonomy struct {
	Modules       []ModuleSpec `json:"modules"`
	Presentations []string     `json:"presentations,omitempty"`
	DataKinds     []string     `json:"dataKinds,omitempty"`
	EntityTypes   []string     `json:"entityTypes,omitempty"`
}

// Request is the input to Decide.
type Request struct {
	Product  string        `json:"product"`
	Text     string        `json:"text"`
	Taxonomy Taxonomy      `json:"taxonomy"`
	State    session.State `json:"state"` // entity refs only, no rendered data
	// Recent is a short tail of the transcript for continuations.
	Recent []string  `json:"recent,omitempty"`
	Now    time.Time `json:"now"`
	TZ     string    `json:"tz,omitempty"`
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
// answer the PREVIOUS turn's pending action rather than naming a module, so
// a deterministic rules.Provider handling them has nothing module-shaped to
// report. See REQ: rules-friendly-validation.
var moduleOptionalInteractions = map[Interaction]bool{
	InteractionConfirmation: true,
	InteractionRejection:    true,
	InteractionCancellation: true,
	InteractionUndo:         true,
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
	Provider string        `json:"provider"`
	Outcome  string        `json:"outcome"` // decided | abstained | low_confidence | invalid | error | timeout
	Detail   string        `json:"detail,omitempty"`
	Latency  time.Duration `json:"latency"`
}

// Trace is the chain's diagnostic record.
type Trace struct {
	DecidedBy string    `json:"decidedBy,omitempty"` // "" when every provider abstained
	Attempts  []Attempt `json:"attempts"`
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
		d, ok, err := p.Decide(pctx, req)
		a := Attempt{Provider: p.Name(), Latency: time.Since(start)}
		// errors.Is (not ==) so a provider that wraps ctx.Err() (e.g.
		// fmt.Errorf("...: %w", ctx.Err())) still classifies as a timeout
		// rather than a generic error.
		timedOut := err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(pctx.Err(), context.DeadlineExceeded))
		cancel()
		switch {
		case err != nil && timedOut:
			a.Outcome, a.Detail = "timeout", err.Error()
		case err != nil:
			a.Outcome, a.Detail = "error", err.Error()
		case !ok:
			a.Outcome = "abstained"
		default:
			if verr := Validate(d, req.Taxonomy); verr != nil {
				a.Outcome, a.Detail = "invalid", verr.Error()
			} else if lowConfidence(d, minConf) {
				a.Outcome = "low_confidence"
				a.Detail = fmt.Sprintf("module=%.2f intent=%.2f", d.Module.Confidence, d.Intent.Confidence)
			} else {
				a.Outcome = "decided"
			}
		}
		tr.Attempts = append(tr.Attempts, a)
		if a.Outcome == "decided" {
			tr.DecidedBy = a.Provider
			return d, true, tr
		}
		if ctx.Err() != nil {
			break
		}
	}
	return Decision{}, false, tr
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
