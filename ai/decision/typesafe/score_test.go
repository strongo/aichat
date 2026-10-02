package typesafe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai/decision"
)

func libraryRequest() decision.ScoreRequest {
	tables := []string{"books", "authors", "members", "loans"}
	var cands []decision.Candidate
	for _, t := range tables {
		cands = append(cands, decision.Candidate{ID: t})
	}
	return decision.ScoreRequest{
		Text: "Which members borrow the most books relative to how long they have been members?",
		Questions: []decision.Question{
			{ID: "tables", Kind: decision.KindRelevance, Instructions: "Is the table needed to answer the question?", Candidates: cands},
			{ID: "kind", Kind: decision.KindChoice, Instructions: "What kind of question is this?", NoneID: "other",
				Candidates: []decision.Candidate{{ID: "aggregate", Description: "totals per group"}, {ID: "relate"}, {ID: "other"}}},
		},
	}
}

// libraryResponse is shaped like the live API's answer: a Choice with a
// probability per option (rounded to two places) and one Noul per candidate.
const libraryResponse = `{"model":"jev-1.13.0","answers":{
  "q0.c0":{"type":"noul","noul":0.04},
  "q0.c1":{"type":"noul","noul":0.02},
  "q0.c2":{"type":"noul","noul":0.71},
  "q0.c3":{"type":"noul","noul":0.96},
  "q1":{"type":"choice","choice":"aggregate","confidence":0.74,"probabilities":{"aggregate":0.8,"relate":0.15,"other":0.05}}
 },"usage":{"input_tokens":512,"output_tokens":127}}`

func TestScore_MapsQuestionsToOneCallAndAnswersBack(t *testing.T) {
	d := &fakeDoer{body: libraryResponse}
	var ev CallEvent
	c := newClient(t, d, func(c *Config) { c.Name = "jev"; c.OnCall = func(e CallEvent) { ev = e } })
	res, err := c.Score(context.Background(), libraryRequest())
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 || ev.Questions != 5 {
		t.Fatalf("calls=%d questions=%d: every question goes in one call", d.calls, ev.Questions)
	}

	// What was sent: the question and the candidate catalogue as the state, one
	// Noul per candidate, one Choice.
	st := d.sent(t)["state"].(map[string]any)
	if st["question"] != "Which members borrow the most books relative to how long they have been members?" || st["context"] != nil {
		t.Fatalf("state = %v", st)
	}
	catalogue := st["candidates"].(map[string]any)["q0"].([]any)
	if len(catalogue) != 4 || catalogue[3].(map[string]any)["name"] != "loans" || catalogue[3].(map[string]any)["description"] != nil {
		t.Fatalf("catalogue = %v", catalogue)
	}
	if _, ok := st["candidates"].(map[string]any)["q1"]; ok {
		t.Fatal("a choice question's options are criteria, not part of the catalogue")
	}
	q := d.questions(t)
	if len(q) != 5 || q["q1"]["type"] != "choice" || !strings.Contains(q["q1"]["instructions"].(string), "`state.question`") {
		t.Fatalf("questions = %v", keys(q))
	}
	noul := q["q0.c3"]
	instr := noul["instructions"].(string)
	if noul["type"] != "noul" || !strings.Contains(instr, "Is the table needed") || !strings.Contains(instr, "`state.candidates.q0[3]`") || !strings.Contains(instr, "`state.question`") {
		t.Fatalf("noul = %v", noul)
	}
	crit := q["q1"]["criteria"].(map[string]any)
	if crit["aggregate"] != "totals per group" || crit["relate"] != nil {
		t.Fatalf("choice criteria = %v", crit)
	}

	// What came back.
	if res.Engine != "jev" || res.Model != "jev-1.13.0" || res.Usage != (decision.Usage{InputTokens: 512, OutputTokens: 127}) {
		t.Fatalf("result = %+v", res)
	}
	rel := res.Answers["tables"]
	if !rel.Calibrated || rel.HasConfidence || rel.Kind != decision.KindRelevance {
		t.Fatalf("relevance answer = %+v", rel)
	}
	var got []string
	for _, s := range rel.Scores {
		got = append(got, fmt.Sprintf("%s=%.2f", s.ID, s.Probability))
	}
	if !reflect.DeepEqual(got, []string{"loans=0.96", "members=0.71", "books=0.04", "authors=0.02"}) {
		t.Fatalf("scores = %v", got)
	}
	ch := res.Answers["kind"]
	if !ch.Calibrated || !ch.HasConfidence || ch.Confidence != 0.74 || ch.NoneID != "other" || ch.Scores[0].ID != "aggregate" {
		t.Fatalf("choice answer = %+v", ch)
	}

	// The documented policy reads it: loans and members relevant, the rest dropped.
	pol := decision.NarrowingPolicy()
	if sel := pol.Evaluate(rel); sel.Outcome != decision.OutcomeSeveral || !reflect.DeepEqual(sel.Picks, []string{"loans", "members"}) {
		t.Fatalf("selection = %+v", sel)
	}
	if sel := pol.Evaluate(ch); sel.Outcome != decision.OutcomeSelected {
		t.Fatalf("selection = %+v", sel)
	}
}

func TestScore_ContextTravelsWithTheText(t *testing.T) {
	d := &fakeDoer{body: libraryResponse}
	req := libraryRequest()
	req.Context = map[string]any{"tables": map[string]any{"loans": []string{"due_on", "member_id"}}}
	if _, err := newClient(t, d).Score(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	st := d.sent(t)["state"].(map[string]any)
	if st["question"] == nil || st["context"].(map[string]any)["tables"] == nil {
		t.Fatalf("state = %v", st)
	}
}

func TestScore_CandidateDescriptionsAreSent(t *testing.T) {
	d := &fakeDoer{body: libraryResponse}
	req := libraryRequest()
	req.Questions[0].Candidates[3].Description = "loans(loan_id, member_id, book_id, due_on)"
	if _, err := newClient(t, d).Score(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	cand := d.sent(t)["state"].(map[string]any)["candidates"].(map[string]any)["q0"].([]any)[3].(map[string]any)
	if cand["description"] != "loans(loan_id, member_id, book_id, due_on)" {
		t.Fatalf("candidate = %v", cand)
	}
}

func TestScore_RejectsBadRequestsWithoutCalling(t *testing.T) {
	d := &fakeDoer{body: libraryResponse}
	c := newClient(t, d)
	if _, err := c.Score(context.Background(), decision.ScoreRequest{}); err == nil || d.calls != 0 {
		t.Fatalf("empty request: err=%v calls=%d", err, d.calls)
	}
	req := libraryRequest()
	req.Questions = req.Questions[1:]
	var many []decision.Candidate
	for i := 0; i <= MaxChoiceOptions; i++ {
		many = append(many, decision.Candidate{ID: fmt.Sprintf("c%d", i)})
	}
	req.Questions[0].Candidates = many
	req.Questions[0].NoneID = ""
	if _, err := c.Score(context.Background(), req); !errors.Is(err, ErrTooManyOptions) || d.calls != 0 {
		t.Fatalf("256 options: err=%v calls=%d", err, d.calls)
	}
	// Exactly the limit is fine to build (the response then fails as bad: not asserted here).
	req.Questions[0].Candidates = many[:MaxChoiceOptions]
	d.body = `{"model":"m","answers":{}}`
	if _, err := c.Score(context.Background(), req); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("255 options: err=%v", err)
	}
}

func TestScore_APIErrorsPropagate(t *testing.T) {
	_, err := newClient(t, &fakeDoer{status: 429}).Score(context.Background(), libraryRequest())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
}

func TestScore_MalformedAnswersAreBadResponses(t *testing.T) {
	const kind = `"q1":{"type":"choice","choice":"aggregate","confidence":0.74,"probabilities":{"aggregate":0.8,"relate":0.15,"other":0.05}}`
	nouls := `"q0.c0":{"type":"noul","noul":0.1},"q0.c1":{"type":"noul","noul":0.1},"q0.c2":{"type":"noul","noul":0.1},"q0.c3":{"type":"noul","noul":0.1}`
	wrap := func(s string) string { return `{"model":"m","answers":{` + s + `}}` }
	cases := map[string]string{
		"choice missing":         wrap(nouls),
		"choice wrong type":      wrap(nouls + `,"q1":{"type":"noul","noul":0.5}`),
		"choice no confidence":   wrap(nouls + `,"q1":{"type":"choice","choice":"relate","probabilities":{"relate":1.0}}`),
		"noul missing":           wrap(strings.Replace(nouls, `"q0.c3":{"type":"noul","noul":0.1}`, `"q9":{"type":"noul","noul":0.1}`, 1) + "," + kind),
		"noul wrong type":        wrap(strings.Replace(nouls, `"q0.c3":{"type":"noul","noul":0.1}`, `"q0.c3":{"type":"choice"}`, 1) + "," + kind),
		"unknown option":         wrap(nouls + `,"q1":{"type":"choice","choice":"x","confidence":0.5,"probabilities":{"x":1.0}}`),
		"probabilities not sum1": wrap(nouls + `,"q1":{"type":"choice","choice":"relate","confidence":0.5,"probabilities":{"relate":0.3,"other":0.1}}`),
		"probability above 1":    wrap(strings.Replace(nouls, `"noul":0.1`, `"noul":1.7`, 1) + "," + kind),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(t, &fakeDoer{body: body}).Score(context.Background(), libraryRequest())
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
