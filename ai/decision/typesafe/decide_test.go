package typesafe

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/session"
)

func taxonomy() decision.Taxonomy {
	return decision.Taxonomy{
		Modules: []decision.ModuleSpec{
			{Name: "calendar", Intents: []string{"show", "create"}, Scopes: []string{"calendar", "calendar.happenings"}},
			{Name: "contacts", Intents: []string{"find"}},
		},
		Presentations: []string{"day_calendar", "list"},
		DataKinds:     []string{"relevant_happenings"},
		EntityTypes:   []string{"happening", "contact"},
		Descriptions:  map[string]string{"calendar/show": "show the calendar", "list": "a plain list"},
	}
}

func decideRequest() decision.Request {
	return decision.Request{
		Product:       "p",
		InteractionID: "interaction-sentinel-123",
		ClientContext: &ai.ClientContext{},
		Text:          "show me friday",
		Taxonomy:      taxonomy(),
	}
}

// answers builds a canned response body from per-question JSON fragments.
func response(parts ...string) string {
	return `{"model":"jev-1.13.0","answers":{` + strings.Join(parts, ",") + `},"usage":{"input_tokens":100,"output_tokens":20}}`
}

func choiceJSON(key, top string, conf float64, probs string) string {
	return `"` + key + `":{"type":"choice","choice":"` + top + `","confidence":` + ftoa(conf) + `,"probabilities":{` + probs + `}}`
}

func noulJSON(key string, p float64) string {
	return `"` + key + `":{"type":"noul","noul":` + ftoa(p) + `}`
}

func ftoa(f float64) string {
	b, _ := jsonFloat(f)
	return b
}

// interactionAnswer answers the interaction question (always asked).
func interactionAnswer(top string, conf float64) string {
	return choiceJSON("interaction", top, conf, `"`+top+`":0.9,"command":0.1`)
}

// fullAnswers answers every question the full taxonomy implies.
func fullAnswers() []string {
	return append(baseAnswers(), interactionAnswer("question", 0.9))
}

// baseAnswers answers every question except the interaction one.
func baseAnswers() []string {
	return []string{
		choiceJSON("intent", "calendar/show", 0.82, `"calendar/show":0.9,"calendar/create":0.05,"contacts/find":0.03,"other":0.02`),
		noulJSON("scope0", 0.95), noulJSON("scope1", 0.2), noulJSON("scope2", 0.9),
		noulJSON("data0", 0.7),
		choiceJSON("entity", "happening", 0.8, `"happening":0.9,"contact":0.05,"none":0.05`),
		choiceJSON("presentation", "day_calendar", 0.9, `"day_calendar":0.95,"list":0.03,"none":0.02`),
	}
}

func TestDecide_FullTaxonomyFoldsIntoADecision(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	c := newClient(t, d)
	var seen []CallEvent
	c.cfg.OnCall = func(e CallEvent) { seen = append(seen, e) }

	dec, ok, err := c.Decide(context.Background(), decideRequest())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := decision.Decision{
		Module:         decision.Scored{Value: "calendar", Confidence: 0.82},
		Intent:         decision.Scored{Value: "show", Confidence: 0.82},
		Interaction:    decision.InteractionQuestion,
		Reference:      &decision.Reference{Kind: "happening"},
		RequiredScopes: []string{"calendar"},
		RequiredData:   []string{"relevant_happenings"},
		Presentation:   "day_calendar",
		NeedsLLM:       true,
		Calibrated:     true,
		Model:          "jev-1.13.0",
		Scores:         map[string]float64{"calendar/show": 0.9, "calendar/create": 0.05, "contacts/find": 0.03, "other": 0.02},
	}
	if !reflect.DeepEqual(dec, want) {
		t.Fatalf("decision = %+v\nwant       %+v", dec, want)
	}
	if err := decision.Validate(dec, taxonomy()); err != nil {
		t.Fatalf("the folded decision must validate: %v", err)
	}
	if len(seen) != 1 || seen[0].Questions != 8 {
		t.Fatalf("one call carries every question: %+v", seen)
	}

	// The question set and what the model is shown.
	q := d.questions(t)
	if len(q) != 8 {
		t.Fatalf("questions = %v", keys(q))
	}
	crit := q["intent"]["criteria"].(map[string]any)
	if crit["calendar/show"] != "show the calendar" || crit["calendar/create"] != nil || crit["contacts/find"] != nil || crit["other"] == nil {
		t.Fatalf("intent criteria = %v", crit)
	}
	if crit2 := q["presentation"]["criteria"].(map[string]any); crit2["list"] != "a plain list" || crit2["none"] == nil {
		t.Fatalf("presentation criteria = %v", crit2)
	}
	// scope0 = calendar, scope1 = calendar.happenings, scope2 = contacts (its own name).
	if got := q["scope2"]["instructions"].(map[string]any)["context"].(map[string]any)["name"]; got != "contacts" {
		t.Fatalf("scope2 = %v", got)
	}
}

func TestDecide_NeverForwardsInteractionIDOrClientContext(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	req := decideRequest()
	req.TZ = "Europe/Dublin"
	if _, _, err := newClient(t, d).Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := string(d.reqBody)
	for _, banned := range []string{"interaction-sentinel-123", "interactionId", "clientContext", "Europe/Dublin", "tz"} {
		if strings.Contains(body, banned) {
			t.Fatalf("request body contains %q: %s", banned, body)
		}
	}
	if !strings.Contains(body, "show me friday") {
		t.Fatal("the message itself must be sent")
	}
}

func TestDecide_StateCarriesTitlesRecentAndContext(t *testing.T) {
	req := decideRequest()
	req.Recent = []string{"what's on tomorrow?"}
	req.Context = map[string]any{"fields": []string{"Total"}}
	req.State = session.State{
		Focused:   &session.EntityRef{Type: "happening", Title: "Dentist"},
		Selection: []session.EntityRef{{Type: "contact", Title: "Ann"}},
		Sidebar:   []session.EntityRef{{Type: "contact", Title: "Bob"}},
		Previous:  &session.Action{Kind: "create"},
	}
	d := &fakeDoer{body: response(append(baseAnswers(), choiceJSON("interaction", "continuation", 0.9, `"continuation":0.95,"question":0.05`))...)}
	dec, ok, err := newClient(t, d).Decide(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if dec.Interaction != decision.InteractionContinuation {
		t.Fatalf("interaction = %q", dec.Interaction)
	}
	st := d.sent(t)["state"].(map[string]any)
	if st["message"] != "show me friday" || len(st["recent"].([]any)) != 1 || st["context"].(map[string]any)["fields"] == nil {
		t.Fatalf("state = %v", st)
	}
	refs := st["session"].([]any)
	if len(refs) != 4 || !strings.Contains(refs[0].(string), "focused: happening") || !strings.Contains(refs[3].(string), "previous action: create") {
		t.Fatalf("session = %v", refs)
	}
	if _, ok := d.questions(t)["interaction"]; !ok {
		t.Fatal("interaction question missing although state is present")
	}
}

// The interaction is never assumed: the question is asked on an empty state
// too, and the model's top option is the Interaction (the Decision has no
// per-interaction confidence, so even a doubtful one is used).
func TestDecide_InteractionIsAlwaysAskedAndTheTopOptionIsUsed(t *testing.T) {
	d := &fakeDoer{body: response(append(baseAnswers(), choiceJSON("interaction", "chat", 0.1, `"chat":0.4,"question":0.35,"command":0.25`))...)}
	dec, ok, err := newClient(t, d).Decide(context.Background(), decideRequest())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	q, asked := d.questions(t)["interaction"]
	if !asked || len(q["criteria"].(map[string]any)) != len(interactionOrder) {
		t.Fatalf("the interaction question must be asked on an empty state: %v", q)
	}
	if dec.Interaction != decision.InteractionChat {
		t.Fatalf("interaction = %q", dec.Interaction)
	}
}

func TestDecide_AnInteractionOutsideTheEnumIsABadResponse(t *testing.T) {
	d := &fakeDoer{body: response(append(baseAnswers(), choiceJSON("interaction", "SECRET-ECHO", 0.9, `"SECRET-ECHO":1`))...)}
	_, _, err := newClient(t, d).Decide(context.Background(), decideRequest())
	if !errors.Is(err, ErrBadResponse) || strings.Contains(err.Error(), "SECRET-ECHO") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecide_OtherAbstains(t *testing.T) {
	d := &fakeDoer{body: response(choiceJSON("intent", "other", 0.9, `"other":0.95,"calendar/show":0.05`))}
	dec, ok, err := newClient(t, d).Decide(context.Background(), decideRequest())
	if ok || err != nil || !reflect.DeepEqual(dec, decision.Decision{}) {
		t.Fatalf("dec=%+v ok=%v err=%v", dec, ok, err)
	}
}

func TestDecide_UnclearSecondaryChoicesAreLeftUnset(t *testing.T) {
	req := decideRequest()
	d := &fakeDoer{body: response(
		choiceJSON("intent", "contacts/find", 0.7, `"contacts/find":0.8,"other":0.2`),
		noulJSON("scope0", 0.1), noulJSON("scope1", 0.1), noulJSON("scope2", 0.61),
		noulJSON("data0", 0.59),
		choiceJSON("entity", "none", 0.9, `"none":0.95,"contact":0.05`),
		choiceJSON("presentation", "list", 0.2, `"list":0.4,"day_calendar":0.35,"none":0.25`), // low confidence
		interactionAnswer("question", 0.9),
	)}
	dec, ok, err := newClient(t, d).Decide(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if dec.Reference != nil || dec.Presentation != "" || dec.Interaction != decision.InteractionQuestion {
		t.Fatalf("decision = %+v", dec)
	}
	// contacts has no Scopes: its own name is the scope; 0.61 reaches 0.60.
	if !reflect.DeepEqual(dec.RequiredScopes, []string{"contacts"}) || dec.RequiredData != nil {
		t.Fatalf("scopes=%v data=%v", dec.RequiredScopes, dec.RequiredData)
	}
	if dec.Module.Value != "contacts" || dec.Intent.Value != "find" {
		t.Fatalf("decision = %+v", dec)
	}
}

func TestDecide_MissingScopeAnswersNeverSelect(t *testing.T) {
	d := &fakeDoer{body: response(
		choiceJSON("intent", "calendar/show", 0.8, `"calendar/show":0.9,"other":0.1`),
		`"scope0":{"type":"choice"}`, // wrong type for a noul: ignored
		choiceJSON("entity", "none", 0.9, `"none":1.0`),
		choiceJSON("presentation", "none", 0.9, `"none":1.0`),
		interactionAnswer("question", 0.9),
	)}
	dec, ok, err := newClient(t, d).Decide(context.Background(), decideRequest())
	if err != nil || !ok || dec.RequiredScopes != nil || dec.RequiredData != nil {
		t.Fatalf("dec=%+v ok=%v err=%v", dec, ok, err)
	}
}

func TestDecide_ChooseRoleIsOneChoice(t *testing.T) {
	req := decision.Request{
		Text: "Which field holds the due dates?",
		Taxonomy: decision.Taxonomy{
			Modules:       []decision.ModuleSpec{{Name: "choose:measure", Intents: []string{"loans.due_on", "count_rows", "none"}}},
			Presentations: []string{"ignored"}, EntityTypes: []string{"ignored"},
			Descriptions: map[string]string{"choose:measure/loans.due_on": "the date a loan is due"},
		},
		Recent: []string{"ignored because roles ask one question"},
	}
	d := &fakeDoer{body: response(choiceJSON("intent", "loans.due_on", 0.9, `"loans.due_on":0.93,"count_rows":0.05,"none":0.02`))}
	dec, ok, err := newClient(t, d).Decide(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if q := d.questions(t); len(q) != 1 {
		t.Fatalf("questions = %v", keys(q))
	}
	crit := d.questions(t)["intent"]["criteria"].(map[string]any)
	if len(crit) != 3 || crit["loans.due_on"] != "the date a loan is due" {
		t.Fatalf("criteria = %v (no `other` is added in role mode)", crit)
	}
	if dec.Module.Value != "choose:measure" || dec.Intent.Value != "loans.due_on" || dec.Interaction != decision.InteractionQuestion {
		t.Fatalf("decision = %+v", dec)
	}
	if err := decision.Validate(dec, req.Taxonomy); err != nil {
		t.Fatal(err)
	}
}

func TestDecide_ModuleWithoutIntents(t *testing.T) {
	req := decision.Request{Text: "hi", Taxonomy: decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "chat"}}}}
	d := &fakeDoer{body: response(choiceJSON("intent", "chat", 0.9, `"chat":0.9,"other":0.1`), noulJSON("scope0", 0.9), interactionAnswer("question", 0.9))}
	dec, ok, err := newClient(t, d).Decide(context.Background(), req)
	if err != nil || !ok || dec.Module.Value != "chat" || dec.Intent.Value != "" || !reflect.DeepEqual(dec.RequiredScopes, []string{"chat"}) {
		t.Fatalf("dec=%+v ok=%v err=%v", dec, ok, err)
	}
}

func TestDecide_ErrorsAndBadAnswers(t *testing.T) {
	// API error propagates and is not an abstention.
	d := &fakeDoer{status: 529}
	_, ok, err := newClient(t, d).Decide(context.Background(), decideRequest())
	if ok || !errors.Is(err, ErrOverloaded) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	bad := map[string]string{
		"missing intent":     response(noulJSON("scope0", 0.9)),
		"intent is a noul":   response(noulJSON("intent", 0.9)),
		"unknown option":     response(choiceJSON("intent", "weather/show", 0.9, `"weather/show":1.0`)),
		"no confidence":      response(`"intent":{"type":"choice","choice":"contacts/find","probabilities":{"contacts/find":1.0}}`),
		"entity missing":     response(choiceJSON("intent", "contacts/find", 0.9, `"contacts/find":1.0`)),
		"presentation wrong": response(choiceJSON("intent", "contacts/find", 0.9, `"contacts/find":1.0`), choiceJSON("entity", "none", 0.9, `"none":1.0`)),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			_, ok, err := newClient(t, &fakeDoer{body: body}).Decide(context.Background(), decideRequest())
			if ok || !errors.Is(err, ErrBadResponse) {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
		})
	}
	// The interaction answer missing although it is always asked.
	_, ok, err = newClient(t, &fakeDoer{body: response(baseAnswers()...)}).Decide(context.Background(), decideRequest())
	if ok || !errors.Is(err, ErrBadResponse) {
		t.Fatalf("interaction missing: ok=%v err=%v", ok, err)
	}
	// An error never quotes the model's own words back.
	_, _, err = newClient(t, &fakeDoer{body: response(choiceJSON("intent", "SECRET-ECHO", 0.9, `"SECRET-ECHO":1.0`))}).Decide(context.Background(), decideRequest())
	if !errors.Is(err, ErrBadResponse) || strings.Contains(err.Error(), "SECRET-ECHO") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecide_PolicyIsConfigurable(t *testing.T) {
	// Presentation confidence 0.7 is selected under narrowing, not under durable.
	body := response(
		choiceJSON("intent", "contacts/find", 0.9, `"contacts/find":0.9,"other":0.1`),
		choiceJSON("entity", "none", 0.9, `"none":1.0`),
		choiceJSON("presentation", "list", 0.7, `"list":0.8,"none":0.2`),
		interactionAnswer("question", 0.9),
	)
	dec, _, err := newClient(t, &fakeDoer{body: body}).Decide(context.Background(), decideRequest())
	if err != nil || dec.Presentation != "list" {
		t.Fatalf("narrowing: %+v %v", dec, err)
	}
	c := newClient(t, &fakeDoer{body: body})
	c.SetPolicy(decision.DurablePolicy())
	dec, _, err = c.Decide(context.Background(), decideRequest())
	if err != nil || dec.Presentation != "" {
		t.Fatalf("durable: %+v %v", dec, err)
	}
}

func TestDecide_WorksInsideAChain(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	pol := decision.NarrowingPolicy()
	got, ok, tr := decision.Chain{Providers: []decision.Provider{newClient(t, d, func(c *Config) { c.Name = "jev" })}, Policy: &pol}.
		Decide(context.Background(), decideRequest())
	if !ok || got.Outcome != decision.OutcomeSelected || tr.DecidedBy != "jev" || !tr.Calibrated {
		t.Fatalf("ok=%v d=%+v tr=%+v", ok, got, tr)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
