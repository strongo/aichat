package llmdecider

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

func TestDecisionSchema_IsValidJSON(t *testing.T) {
	if !json.Valid([]byte(decisionSchema)) {
		t.Fatal("decisionSchema is not valid JSON")
	}
}

func TestDecisionSchema_MatchesFields(t *testing.T) {
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(decisionSchema), &parsed); err != nil {
		t.Fatal(err)
	}
	want := []string{"module", "intent", "interaction", "reference", "requiredScopes", "requiredData", "slots", "canHandleDeterministically", "needsLLM", "presentation"}
	for _, f := range want {
		if _, ok := parsed.Properties[f]; !ok {
			t.Errorf("schema missing property %q", f)
		}
	}
	// Round-trip a fully populated Decision through the field names the
	// schema declares, to catch a renamed json tag drifting out of sync.
	d := decision.Decision{
		Module:                     decision.Scored{Value: "calendar", Confidence: 0.9},
		Intent:                     decision.Scored{Value: "show", Confidence: 0.9},
		Interaction:                decision.InteractionCommand,
		Reference:                  &decision.Reference{Kind: "happening", Expression: "it", Pronoun: true},
		RequiredScopes:             []string{"calendar"},
		RequiredData:               []string{"relevant_happenings"},
		Slots:                      map[string]string{"when": "friday"},
		CanHandleDeterministically: true,
		NeedsLLM:                   false,
		Presentation:               "day_calendar",
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(b, &asMap); err != nil {
		t.Fatal(err)
	}
	for _, f := range want {
		if f == "presentation" || f == "reference" {
			continue // omitempty fields, always present here so fine either way
		}
		if _, ok := asMap[f]; !ok {
			t.Errorf("Decision JSON missing field %q that the schema declares", f)
		}
	}
}

// fakeLLM is a minimal ai.LLMProvider that either streams pre-canned events
// or captures the request it was called with.
type fakeLLM struct {
	events  []ai.Event
	err     error
	lastReq ai.ChatRequest
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) Stream(ctx context.Context, req ai.ChatRequest) iter.Seq2[ai.Event, error] {
	f.lastReq = req
	return func(yield func(ai.Event, error) bool) {
		if f.err != nil {
			yield(ai.Event{}, f.err)
			return
		}
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func taxonomy() decision.Taxonomy {
	return decision.Taxonomy{
		Modules: []decision.ModuleSpec{
			{Name: "calendar", Intents: []string{"show", "create"}, Scopes: []string{"calendar"}},
		},
		Presentations: []string{"day_calendar"},
	}
}

func TestDecide_ParsesStructuredEvent(t *testing.T) {
	raw := json.RawMessage(`{"module":{"value":"calendar","confidence":0.9},"intent":{"value":"show","confidence":0.8},"interaction":"command","canHandleDeterministically":true,"needsLLM":false}`)
	llm := &fakeLLM{events: []ai.Event{
		{Type: ai.EventStarted},
		{Type: ai.EventTextDelta, Text: "some prose"},
		{Type: ai.EventStructured, Structured: raw},
		{Type: ai.EventCompleted},
	}}
	dec := New(llm, Options{})
	if dec.Name() != "llm-decider" {
		t.Fatalf("Name() = %q", dec.Name())
	}
	d, ok, err := dec.Decide(context.Background(), decision.Request{Text: "show my calendar", Taxonomy: taxonomy()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ok || d.Module.Value != "calendar" || d.Intent.Value != "show" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if llm.lastReq.ResponseSchema == nil {
		t.Fatal("expected ResponseSchema to be set")
	}
	if !strings.Contains(llm.lastReq.System, "calendar") {
		t.Errorf("system prompt should mention the taxonomy: %q", llm.lastReq.System)
	}
}

func TestDecide_FallsBackToParsingText(t *testing.T) {
	llm := &fakeLLM{events: []ai.Event{
		{Type: ai.EventTextDelta, Text: "Here is the answer:\n```json\n"},
		{Type: ai.EventTextDelta, Text: `{"module":{"value":"calendar","confidence":0.9},"intent":{"value":"show","confidence":0.8},"interaction":"command","canHandleDeterministically":true,"needsLLM":false}`},
		{Type: ai.EventTextDelta, Text: "\n```"},
		{Type: ai.EventCompleted},
	}}
	dec := New(llm, Options{})
	d, ok, err := dec.Decide(context.Background(), decision.Request{Text: "show my calendar", Taxonomy: taxonomy()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ok || d.Module.Value != "calendar" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
}

func TestDecide_MalformedJSONIsError(t *testing.T) {
	llm := &fakeLLM{events: []ai.Event{
		{Type: ai.EventTextDelta, Text: "not json at all"},
		{Type: ai.EventCompleted},
	}}
	dec := New(llm, Options{})
	_, ok, err := dec.Decide(context.Background(), decision.Request{Text: "x", Taxonomy: taxonomy()})
	if err == nil {
		t.Fatal("expected error for unparseable output")
	}
	if ok {
		t.Fatal("expected ok=false on error")
	}
}

func TestDecide_StreamErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	llm := &fakeLLM{err: wantErr}
	dec := New(llm, Options{})
	_, ok, err := dec.Decide(context.Background(), decision.Request{Text: "x", Taxonomy: taxonomy()})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
	if ok {
		t.Fatal("expected ok=false on error")
	}
}

func TestDecide_NeverInventsID_PromptMentionsRule(t *testing.T) {
	llm := &fakeLLM{events: []ai.Event{{Type: ai.EventCompleted}}}
	dec := New(llm, Options{})
	_, _, _ = dec.Decide(context.Background(), decision.Request{Text: "x", Taxonomy: taxonomy()})
	if !strings.Contains(llm.lastReq.System, "Never invent entity IDs") {
		t.Error("system prompt must instruct the model never to invent entity IDs")
	}
	if !strings.Contains(llm.lastReq.System, "MINIMUM") {
		t.Error("system prompt must instruct minimum required scopes")
	}
}

func TestDecide_SessionStateAndRecentInContext(t *testing.T) {
	llm := &fakeLLM{events: []ai.Event{{Type: ai.EventCompleted}}}
	dec := New(llm, Options{MaxRecent: 2})
	req := decision.Request{
		Text:     "x",
		Taxonomy: taxonomy(),
		Recent:   []string{"turn1", "turn2", "turn3"},
	}
	_, _, _ = dec.Decide(context.Background(), req)
	if len(llm.lastReq.Context) != 1 {
		t.Fatalf("Context = %+v", llm.lastReq.Context)
	}
	block := llm.lastReq.Context[0]
	if strings.Contains(block.Text, "turn1") {
		t.Errorf("MaxRecent should have trimmed the oldest line: %q", block.Text)
	}
	if !strings.Contains(block.Text, "turn2") || !strings.Contains(block.Text, "turn3") {
		t.Errorf("expected the last 2 recent lines: %q", block.Text)
	}
}
