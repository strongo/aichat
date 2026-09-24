// Package llmdecider is a decision.Provider that makes ONE structured
// inference against an ai.LLMProvider to produce a decision.Decision. It is
// the product-neutral implementation the cloud hosts as "Jev": nothing here
// is Sneat- or DataTug-specific, only the taxonomy passed in on each Request
// gives it product shape.
package llmdecider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

// Options configures a Decider.
type Options struct {
	// Name identifies this decider in decision.Trace/Attempt (default
	// "llm-decider").
	Name string
	// Model, when set, overrides the LLM provider's default model for this
	// decider's inference only.
	Model string
	// MaxRecent bounds how many trailing Request.Recent lines are sent. The
	// zero value (unset) defaults to 8. A NEGATIVE value means "no limit,
	// send every line" -- 0 itself cannot mean that, since New treats a
	// zero value as "unset" and fills in the default.
	MaxRecent int
}

// Decider implements decision.Provider with one structured LLM inference.
type Decider struct {
	llm  ai.LLMProvider
	opts Options
}

// New builds a Decider backed by llm.
func New(llm ai.LLMProvider, opts Options) *Decider {
	if opts.Name == "" {
		opts.Name = "llm-decider"
	}
	if opts.MaxRecent == 0 {
		opts.MaxRecent = 8
	}
	return &Decider{llm: llm, opts: opts}
}

// decisionTimeout is how long decision.Chain should wait for a Decider call
// (see the optional `DecisionTimeout() time.Duration` hook Chain honours).
// An inference call reasonably wants more time than Chain's 1500ms default.
const decisionTimeout = 4 * time.Second

// Name implements decision.Provider.
func (d *Decider) Name() string { return d.opts.Name }

// DecisionTimeout implements the optional interface decision.Chain honours.
func (d *Decider) DecisionTimeout() time.Duration { return decisionTimeout }

// Decide implements decision.Provider.
func (d *Decider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	chatReq := ai.ChatRequest{
		Model:          d.opts.Model,
		System:         systemPrompt(req.Taxonomy),
		Context:        []ai.ContextBlock{stateContextBlock(req, d.opts.MaxRecent)},
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: req.Text}},
		ResponseSchema: json.RawMessage(decisionSchema),
		Metadata:       map[string]string{"path": "decision"},
	}
	text, structured, _, err := ai.Collect(d.llm.Stream(ctx, chatReq))
	if err != nil {
		return decision.Decision{}, false, fmt.Errorf("llmdecider: inference: %w", err)
	}
	raw := structured
	if len(raw) == 0 {
		raw = json.RawMessage(extractJSON(text))
	}
	if len(raw) == 0 {
		return decision.Decision{}, false, fmt.Errorf("llmdecider: no structured output and no parseable text")
	}
	var w wireDecision
	if err := json.Unmarshal(raw, &w); err != nil {
		return decision.Decision{}, false, fmt.Errorf("llmdecider: malformed decision JSON: %w", err)
	}
	return w.toDecision(), true, nil
}

// wireDecision mirrors decisionSchema's wire shape exactly (see schema.go's
// doc comment for why it differs from decision.Decision: strict structured
// output has no open-map type, so Slots travels as an array).
type wireDecision struct {
	Module                     decision.Scored      `json:"module"`
	Intent                     decision.Scored      `json:"intent"`
	Interaction                decision.Interaction `json:"interaction"`
	Reference                  *wireReference       `json:"reference"`
	RequiredScopes             []string             `json:"requiredScopes"`
	RequiredData               []string             `json:"requiredData"`
	Slots                      []wireSlot           `json:"slots"`
	CanHandleDeterministically bool                 `json:"canHandleDeterministically"`
	NeedsLLM                   bool                 `json:"needsLLM"`
	Presentation               *string              `json:"presentation"`
}

type wireReference struct {
	Kind       string `json:"kind"`
	Expression string `json:"expression"`
	Pronoun    bool   `json:"pronoun"`
}

type wireSlot struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// toDecision converts the wire shape to decision.Decision, folding the
// slots array back into a map and treating an empty Reference.Kind or a nil
// Presentation the same as "not set".
func (w wireDecision) toDecision() decision.Decision {
	d := decision.Decision{
		Module:                     w.Module,
		Intent:                     w.Intent,
		Interaction:                w.Interaction,
		RequiredScopes:             w.RequiredScopes,
		RequiredData:               w.RequiredData,
		CanHandleDeterministically: w.CanHandleDeterministically,
		NeedsLLM:                   w.NeedsLLM,
	}
	if w.Reference != nil && w.Reference.Kind != "" {
		ref := decision.Reference{Kind: w.Reference.Kind, Expression: w.Reference.Expression, Pronoun: w.Reference.Pronoun}
		d.Reference = &ref
	}
	if len(w.Slots) > 0 {
		m := make(map[string]string, len(w.Slots))
		for _, s := range w.Slots {
			if s.Name == "" {
				continue
			}
			m[s.Name] = s.Value
		}
		if len(m) > 0 {
			d.Slots = m
		}
	}
	if w.Presentation != nil {
		d.Presentation = *w.Presentation
	}
	return d
}

// systemPrompt is a compact, product-neutral instruction: the taxonomy the
// model must choose from, and the hard rules that keep a decision safe to
// act on downstream (never invent IDs, minimum scopes, deterministic-only
// when data alone answers it).
func systemPrompt(t decision.Taxonomy) string {
	var b strings.Builder
	b.WriteString("You are a routing classifier for a conversational product. ")
	b.WriteString("Given the user's message, decide: which module and intent it belongs to, ")
	b.WriteString("the kind of interaction, what (if anything) the user refers to, the MINIMUM ")
	b.WriteString("context scopes and dynamic data needed to answer, and whether the product can ")
	b.WriteString("answer deterministically from data alone without a further LLM call.\n\n")

	b.WriteString("Rules:\n")
	b.WriteString("- Never invent entity IDs. `reference` is an EXPRESSION to resolve (\"my dentist appointment tomorrow\"), never an ID.\n")
	b.WriteString("- requiredScopes must be the MINIMUM scopes needed, not every scope that might help.\n")
	b.WriteString("- Set canHandleDeterministically=true and needsLLM=false ONLY when the product can fully answer from data alone with no further model reasoning (e.g. \"show my calendar\" -> just render data).\n")
	b.WriteString("- module and intent must come from the taxonomy below; do not invent new ones.\n")
	b.WriteString("- confidence is your calibrated probability in [0,1] that module/intent are correct.\n\n")

	b.WriteString("Taxonomy:\n")
	for _, m := range t.Modules {
		fmt.Fprintf(&b, "- module %q: intents %v", m.Name, m.Intents)
		if len(m.Scopes) > 0 {
			fmt.Fprintf(&b, "; scopes %v", m.Scopes)
		}
		b.WriteString("\n")
	}
	if len(t.Presentations) > 0 {
		fmt.Fprintf(&b, "Presentations: %v\n", t.Presentations)
	}
	if len(t.DataKinds) > 0 {
		fmt.Fprintf(&b, "Data kinds: %v\n", t.DataKinds)
	}
	if len(t.EntityTypes) > 0 {
		fmt.Fprintf(&b, "Entity types: %v\n", t.EntityTypes)
	}
	return b.String()
}

// stateContextBlock renders the request's session state (entity refs and
// titles only -- never rendered data) and a short recent-turn tail as a
// single dynamic context block, plus now/tz.
func stateContextBlock(req decision.Request, maxRecent int) ai.ContextBlock {
	var b strings.Builder
	if req.State.Focused != nil {
		fmt.Fprintf(&b, "Focused: %s %q\n", req.State.Focused.Type, req.State.Focused.Title)
	}
	for _, s := range req.State.Selection {
		fmt.Fprintf(&b, "Selected: %s %q\n", s.Type, s.Title)
	}
	for _, s := range req.State.Sidebar {
		fmt.Fprintf(&b, "Sidebar: %s %q\n", s.Type, s.Title)
	}
	if req.State.Previous != nil {
		fmt.Fprintf(&b, "Previous action: %s\n", req.State.Previous.Kind)
	}
	recent := req.Recent
	if maxRecent > 0 && len(recent) > maxRecent {
		recent = recent[len(recent)-maxRecent:]
	}
	for _, line := range recent {
		fmt.Fprintf(&b, "Recent: %s\n", line)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	fmt.Fprintf(&b, "Now: %s", now.Format(time.RFC3339))
	if req.TZ != "" {
		fmt.Fprintf(&b, " (tz=%s)", req.TZ)
	}
	return ai.ContextBlock{Scope: "session", Kind: ai.ContextDynamic, Name: "session_state", Text: b.String()}
}

// extractJSON pulls a JSON object out of free text, tolerating a ```json
// fenced block or surrounding prose, for providers/tests that don't set
// EventStructured.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}
