package typesafe

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/session"
)

// otherOption is the "none of these" option added to the intent Choice, as
// TypeSafe advises for a list that may not cover the input.
const otherOption = "other"

// noneOption is the "none" option of the entity and presentation Choices.
const noneOption = "none"

// chooseRolePrefix marks a request that is not a taxonomy decision but one
// closed question over candidates: a single module named "choose:<role>" whose
// Intents are the candidate ids. It becomes exactly one Choice. (New code
// should prefer Score, which carries any number of questions and kinds.)
const chooseRolePrefix = "choose:"

// interactionDescriptions describe the Interaction enum to the model.
var interactionDescriptions = map[decision.Interaction]string{
	decision.InteractionCommand:      "the user asks for something to be done",
	decision.InteractionQuestion:     "the user asks something to be answered",
	decision.InteractionConfirmation: "the user agrees to what was just proposed",
	decision.InteractionRejection:    "the user declines what was just proposed",
	decision.InteractionCorrection:   "the user corrects the previous answer or action",
	decision.InteractionContinuation: "the user refines or continues the previous request",
	decision.InteractionCancellation: "the user abandons the pending request",
	decision.InteractionUndo:         "the user asks to reverse the last action",
	decision.InteractionChat:         "general conversation that needs no product action",
}

// interactionOrder fixes the option order of the interaction Choice.
var interactionOrder = []decision.Interaction{
	decision.InteractionCommand, decision.InteractionQuestion, decision.InteractionConfirmation,
	decision.InteractionRejection, decision.InteractionCorrection, decision.InteractionContinuation,
	decision.InteractionCancellation, decision.InteractionUndo, decision.InteractionChat,
}

// SetPolicy sets the selection policy Decide uses to read the secondary
// choices (entity, presentation, interaction) and the scope and data-kind
// probabilities. The default is decision.NarrowingPolicy. Call it before the
// client is shared between goroutines.
func (c *Client) SetPolicy(p decision.SelectionPolicy) { c.policy = &p }

func (c *Client) selection() decision.SelectionPolicy {
	if c.policy != nil {
		return *c.policy
	}
	return decision.NarrowingPolicy()
}

// optionRef is what an option of the intent Choice stands for.
type optionRef struct{ module, intent string }

// decidePlan is the set of questions built from a request and what each one
// folds back into.
type decidePlan struct {
	questions map[string]Question
	options   map[string]optionRef // intent option -> module/intent
	role      string               // non-empty in choose:<role> mode
	scopes    map[string]string    // wire key -> scope
	data      map[string]string    // wire key -> data kind
	entity    bool
	present   bool
	interact  bool
}

// Decide implements decision.Provider with ONE API call for every question the
// taxonomy implies:
//
//   - one Choice over "module/intent" plus "other" -> Module, Intent, Scores
//     (the Choice's confidence is both confidences); "other" abstains;
//   - one Noul per scope -> RequiredScopes (scopes of the chosen module whose
//     probability reaches the policy's select threshold);
//   - one Noul per data kind -> RequiredData (same threshold);
//   - one Choice over entity types plus "none" -> Reference.Kind, only if the
//     policy selects it;
//   - one Choice over presentations plus "none" -> Presentation, same rule;
//   - one Choice over the Interaction enum, asked only when the request carries
//     session state or recent turns; otherwise Interaction is "question" without
//     asking, because Validate requires a known value.
//
// The state is the message, the recent tail, the entity titles of the session
// state and Request.Context, as one object. InteractionID, ClientContext, Now
// and TZ are never forwarded.
//
// The Decision is Calibrated, carries Scores, and has NeedsLLM set: the model
// says which route to take, not that the product can answer without a language
// model.
func (c *Client) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	plan := c.plan(req)
	resp, err := c.Ask(ctx, AskRequest{State: decideState(req), Questions: plan.questions})
	if err != nil {
		return decision.Decision{}, false, err
	}
	return c.fold(req, plan, resp)
}

func (c *Client) plan(req decision.Request) decidePlan {
	t := req.Taxonomy
	p := decidePlan{questions: map[string]Question{}, options: map[string]optionRef{}, scopes: map[string]string{}, data: map[string]string{}}

	// Intent choice.
	opts := map[string]any{}
	if len(t.Modules) == 1 && strings.HasPrefix(t.Modules[0].Name, chooseRolePrefix) {
		m := t.Modules[0]
		p.role = m.Name
		for _, in := range m.Intents {
			opts[in] = description(t, m.Name+"/"+in)
			p.options[in] = optionRef{module: m.Name, intent: in}
		}
		p.questions["intent"] = Choice("Which of the options best answers the question about the message in `state.message`?", opts)
	} else {
		for _, m := range t.Modules {
			if len(m.Intents) == 0 {
				opts[m.Name] = description(t, m.Name)
				p.options[m.Name] = optionRef{module: m.Name}
			}
			for _, in := range m.Intents {
				key := m.Name + "/" + in
				opts[key] = description(t, key)
				p.options[key] = optionRef{module: m.Name, intent: in}
			}
		}
		opts[otherOption] = "none of the other options fits the message"
		p.questions["intent"] = Choice("Which module and intent does the user's message in `state.message` belong to? Answer `other` if none fits.", opts)
	}

	// Scopes and data kinds: independent yes/no questions.
	if p.role == "" {
		for _, s := range scopeNames(t) {
			key := fmt.Sprintf("scope%d", len(p.scopes))
			p.scopes[key] = s
			p.questions[key] = Noul(map[string]any{
				"question": "Is the `context` needed to answer the user's message in `state.message`?",
				"context":  map[string]any{"name": s, "description": description(t, s)},
			})
		}
		for i, k := range t.DataKinds {
			key := fmt.Sprintf("data%d", i)
			p.data[key] = k
			p.questions[key] = Noul(map[string]any{
				"question": "Must the product fetch the `data` to answer the user's message in `state.message`?",
				"data":     map[string]any{"name": k, "description": description(t, k)},
			})
		}
		if len(t.EntityTypes) > 0 {
			p.entity = true
			e := map[string]any{noneOption: "the message refers to none of these"}
			for _, k := range t.EntityTypes {
				e[k] = description(t, k)
			}
			p.questions["entity"] = Choice("What kind of thing, if any, does the user's message in `state.message` refer to?", e)
		}
		if len(t.Presentations) > 0 {
			p.present = true
			e := map[string]any{noneOption: "no particular presentation is implied"}
			for _, k := range t.Presentations {
				e[k] = description(t, k)
			}
			p.questions["presentation"] = Choice("How should the answer to the message in `state.message` be presented?", e)
		}
		if askInteraction(req) {
			p.interact = true
			e := map[string]any{}
			for _, i := range interactionOrder {
				e[string(i)] = interactionDescriptions[i]
			}
			p.questions["interaction"] = Choice("What kind of turn is the user's message in `state.message`, given the earlier conversation?", e)
		}
	}
	return p
}

// description returns the taxonomy's description of an entry, or nil (JSON
// null, "no extra detail") when it has none.
func description(t decision.Taxonomy, entry string) any {
	if d := t.Descriptions[entry]; d != "" {
		return d
	}
	return nil
}

// scopeNames lists the distinct scopes any module may need, in taxonomy order
// (a module with no Scopes uses its own name, as decision.Validate does).
func scopeNames(t decision.Taxonomy) []string {
	var out []string
	for _, m := range t.Modules {
		for _, s := range moduleScopes(m) {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

func moduleScopes(m decision.ModuleSpec) []string {
	if len(m.Scopes) > 0 {
		return m.Scopes
	}
	return []string{m.Name}
}

func askInteraction(req decision.Request) bool {
	st := req.State
	return len(req.Recent) > 0 || st.Focused != nil || len(st.Selection) > 0 || len(st.Sidebar) > 0 || st.Previous != nil
}

// decideState builds the state object: names and titles only.
func decideState(req decision.Request) map[string]any {
	st := map[string]any{"message": req.Text}
	if len(req.Recent) > 0 {
		st["recent"] = req.Recent
	}
	var refs []string
	add := func(label string, r *session.EntityRef) {
		if r != nil {
			refs = append(refs, fmt.Sprintf("%s: %s %q", label, r.Type, r.Title))
		}
	}
	add("focused", req.State.Focused)
	for i := range req.State.Selection {
		add("selected", &req.State.Selection[i])
	}
	for i := range req.State.Sidebar {
		add("sidebar", &req.State.Sidebar[i])
	}
	if req.State.Previous != nil {
		refs = append(refs, "previous action: "+string(req.State.Previous.Kind))
	}
	if len(refs) > 0 {
		st["session"] = refs
	}
	if len(req.Context) > 0 {
		st["context"] = req.Context
	}
	return st
}

// fold turns the API's answers into a Decision.
func (c *Client) fold(req decision.Request, p decidePlan, resp *AskResponse) (decision.Decision, bool, error) {
	pol := c.selection()
	intent, err := choiceAnswer(resp, "intent")
	if err != nil {
		return decision.Decision{}, false, err
	}
	if intent.top == otherOption {
		return decision.Decision{}, false, nil
	}
	ref, known := p.options[intent.top]
	if !known {
		return decision.Decision{}, false, fmt.Errorf("%w: intent answer %q is not an option", ErrBadResponse, intent.top)
	}
	d := decision.Decision{
		Module:      decision.Scored{Value: ref.module, Confidence: intent.confidence},
		Intent:      decision.Scored{Value: ref.intent, Confidence: intent.confidence},
		Interaction: decision.InteractionQuestion,
		Scores:      intent.probabilities,
		Calibrated:  true,
		NeedsLLM:    true,
	}

	if p.role == "" {
		for _, m := range req.Taxonomy.Modules {
			if m.Name != ref.module {
				continue
			}
			for _, s := range moduleScopes(m) {
				if probabilityOf(resp, p.scopes, s) >= pol.MinProbability {
					d.RequiredScopes = append(d.RequiredScopes, s)
				}
			}
		}
		for _, k := range req.Taxonomy.DataKinds {
			if probabilityOf(resp, p.data, k) >= pol.MinProbability {
				d.RequiredData = append(d.RequiredData, k)
			}
		}
	}

	// Secondary choices are used only when the policy says "selected".
	if p.entity {
		if v, ok, err := selectedChoice(resp, "entity", noneOption, pol); err != nil {
			return decision.Decision{}, false, err
		} else if ok {
			d.Reference = &decision.Reference{Kind: v}
		}
	}
	if p.present {
		if v, ok, err := selectedChoice(resp, "presentation", noneOption, pol); err != nil {
			return decision.Decision{}, false, err
		} else if ok {
			d.Presentation = v
		}
	}
	if p.interact {
		if v, ok, err := selectedChoice(resp, "interaction", "", pol); err != nil {
			return decision.Decision{}, false, err
		} else if ok {
			d.Interaction = decision.Interaction(v)
		}
	}
	return d, true, nil
}

type choiceView struct {
	top           string
	confidence    float64
	probabilities map[string]float64
	answer        Answer
}

func choiceAnswer(resp *AskResponse, key string) (choiceView, error) {
	a, ok := resp.Answers[key]
	if !ok || a.Type != TypeChoice || a.Probabilities == nil || a.Confidence == nil {
		return choiceView{}, fmt.Errorf("%w: expected a choice answer for %q", ErrBadResponse, key)
	}
	return choiceView{top: a.Choice, confidence: *a.Confidence, probabilities: a.Probabilities, answer: a}, nil
}

// selectedChoice returns the top option of a secondary Choice when the policy
// selects it and it is not the none option.
func selectedChoice(resp *AskResponse, key, none string, pol decision.SelectionPolicy) (string, bool, error) {
	v, err := choiceAnswer(resp, key)
	if err != nil {
		return "", false, err
	}
	scores := make([]decision.Score, 0, len(v.probabilities))
	for id, p := range v.probabilities {
		scores = append(scores, decision.Score{ID: id, Probability: p})
	}
	ans := decision.NewAnswer(key, decision.KindChoice, scores)
	ans.Calibrated, ans.HasConfidence, ans.Confidence, ans.NoneID = true, true, v.confidence, none
	sel := pol.Evaluate(ans)
	if sel.Outcome != decision.OutcomeSelected {
		return "", false, nil
	}
	return sel.Picks[0], true, nil
}

// probabilityOf returns the Noul probability of the question whose wire key
// maps to name (0 when the answer is absent: absence never selects).
func probabilityOf(resp *AskResponse, keys map[string]string, name string) float64 {
	for key, n := range keys {
		if n == name {
			if a, ok := resp.Answers[key]; ok && a.Type == TypeNoul {
				return a.Noul
			}
		}
	}
	return 0
}
