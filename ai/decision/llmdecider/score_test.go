package llmdecider

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

func libraryScoreRequest() decision.ScoreRequest {
	return decision.ScoreRequest{
		Text:    "Which members borrowed the most books last month?",
		Context: map[string]any{"database": "a small lending library"},
		Questions: []decision.Question{
			{ID: "needed", Kind: decision.KindRelevance, Instructions: "Is the table needed?", Candidates: []decision.Candidate{
				{ID: "books", Description: "book_id, title"}, {ID: "loans"}, {ID: "members"}, {ID: "shelves"}}},
			{ID: "primary", Kind: decision.KindChoice, Instructions: "Which single table?", NoneID: "none", Candidates: []decision.Candidate{
				{ID: "books"}, {ID: "loans"}, {ID: "none"}}},
		},
	}
}

func scoresLLM(t *testing.T, body string) *fakeLLM {
	t.Helper()
	return &fakeLLM{events: []ai.Event{
		{Type: ai.EventStructured, Structured: json.RawMessage(body)},
		{Type: ai.EventUsage, Usage: &ai.Usage{InputTokens: 210, OutputTokens: 40}},
		{Type: ai.EventCompleted},
	}}
}

const goodScores = `{"answers":[
  {"questionId":"needed","scores":[{"id":"loans","probability":0.95},{"id":"members","probability":0.9},{"id":"books","probability":0.4},{"id":"shelves","probability":0.02}]},
  {"questionId":"primary","scores":[{"id":"loans","probability":0.7},{"id":"books","probability":0.2},{"id":"none","probability":0.1}]}
]}`

func TestScoreSchema_IsValidAndStrict(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(scoreSchema), &root); err != nil {
		t.Fatalf("scoreSchema is not valid JSON: %v", err)
	}
	walkStrict(t, "$", root)
}

func TestScore_OneInferenceAnswersEveryQuestionUncalibrated(t *testing.T) {
	llm := scoresLLM(t, goodScores)
	dec := New(llm, Options{Name: "haiku", Model: "small-model"})
	req := libraryScoreRequest()
	res, err := dec.Score(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "haiku" || res.Model != "small-model" || res.Usage != (decision.Usage{InputTokens: 210, OutputTokens: 40}) {
		t.Fatalf("result = %+v", res)
	}
	if err := decision.ValidateScoreResult(req, res); err != nil {
		t.Fatalf("the result must validate: %v", err)
	}
	rel, ch := res.Answers["needed"], res.Answers["primary"]
	if rel.Calibrated || rel.HasConfidence || ch.Calibrated || ch.HasConfidence {
		t.Fatalf("an LLM's numbers are uncalibrated and carry no confidence: %+v %+v", rel, ch)
	}
	if rel.Scores[0].ID != "loans" || rel.Scores[0].Probability != 0.95 || ch.NoneID != "none" || ch.Scores[0].ID != "loans" {
		t.Fatalf("answers = %+v %+v", rel, ch)
	}

	// What was sent: one request carrying every question, the context and the
	// strict schema; nothing else.
	r := llm.lastReq
	if r.Model != "small-model" || string(r.ResponseSchema) != scoreSchema || len(r.Messages) != 1 || r.Metadata["path"] != "score" {
		t.Fatalf("request = %+v", r)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(r.Messages[0].Text), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["question"] != req.Text || sent["context"] == nil || len(sent["questions"].([]any)) != 2 {
		t.Fatalf("sent = %v", sent)
	}
}

// The point of an LLM scorer behind Jev: narrowing still works, as a proposal.
func TestScore_UncalibratedScoresBecomeAProposalUnderTheNarrowingPolicy(t *testing.T) {
	dec := New(scoresLLM(t, goodScores), Options{})
	res, err := dec.Score(context.Background(), libraryScoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	pol := decision.NarrowingPolicy()
	sel := pol.Evaluate(res.Answers["needed"])
	if sel.Outcome != decision.OutcomeUnscored || !sel.Proposal || sel.Actionable() || !reflect.DeepEqual(sel.Picks, []string{"loans", "members"}) {
		t.Fatalf("relevance: %+v", sel)
	}
	if sel := pol.Evaluate(res.Answers["primary"]); !sel.Proposal || !reflect.DeepEqual(sel.Picks, []string{"loans"}) {
		t.Fatalf("choice: %+v", sel)
	}
}

func TestScore_NormalisesChoicesClampsAndDropsInventedIds(t *testing.T) {
	const body = `{"answers":[
	  {"questionId":"needed","scores":[{"id":"loans","probability":1.7},{"id":"invented","probability":0.9},{"id":"loans","probability":0.1},{"id":"members","probability":-0.3}]},
	  {"questionId":"primary","scores":[{"id":"loans","probability":0.6},{"id":"books","probability":0.3}]},
	  {"questionId":"needed","scores":[{"id":"books","probability":1}]}
	]}`
	res, err := New(scoresLLM(t, body), Options{}).Score(context.Background(), libraryScoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	rel := res.Answers["needed"] // first answer for the id wins; clamped; first duplicate wins; the rest score 0
	got := map[string]float64{}
	for _, s := range rel.Scores {
		got[s.ID] = s.Probability
	}
	if !reflect.DeepEqual(got, map[string]float64{"loans": 1, "members": 0, "books": 0, "shelves": 0}) {
		t.Fatalf("relevance = %v", got)
	}
	ch := res.Answers["primary"] // 0.6/0.3/0 normalised to sum to 1
	if p, _ := ch.Probability("loans"); p < 0.66 || p > 0.67 {
		t.Fatalf("choice = %+v", ch)
	}
	if p, ok := ch.Probability("none"); !ok || p != 0 {
		t.Fatalf("a candidate the model left out scores 0: %+v", ch)
	}
}

func TestScore_UnusableAnswersAreErrorsThatQuoteNothing(t *testing.T) {
	const secret = "SECRET-CANDIDATE"
	req := libraryScoreRequest()
	req.Questions[0].Candidates[0].ID = secret
	req.Questions[1].Candidates[0].ID = secret
	cases := map[string]string{
		"not the documented shape": `{"answers":"` + secret + `"}`,
		"question unanswered":      `{"answers":[{"questionId":"needed","scores":[{"id":"` + secret + `","probability":1}]}]}`,
		"choice with no mass":      `{"answers":[{"questionId":"needed","scores":[]},{"questionId":"primary","scores":[{"id":"` + secret + `","probability":0}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(scoresLLM(t, body), Options{}).Score(context.Background(), req)
			if !errors.Is(err, ErrBadScores) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "needed") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestScore_RequestAndInferenceErrors(t *testing.T) {
	// An invalid request is the caller's fault and quotes nothing.
	bad := libraryScoreRequest()
	bad.Questions[0].Candidates[1].ID = "dup-secret"
	bad.Questions[0].Candidates[0].ID = "dup-secret"
	llm := scoresLLM(t, goodScores)
	_, err := New(llm, Options{}).Score(context.Background(), bad)
	if !errors.Is(err, decision.ErrInvalidRequest) || strings.Contains(err.Error(), "dup-secret") || llm.lastReq.Messages != nil {
		t.Fatalf("err=%v", err)
	}
	// A context that cannot be encoded is too.
	unencodable := libraryScoreRequest()
	unencodable.Context = map[string]any{"x": make(chan int)}
	if _, err := New(llm, Options{}).Score(context.Background(), unencodable); !errors.Is(err, decision.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	// Stream failures, empty output and a missing LLM are errors, not answers.
	boom := errors.New("boom")
	if _, err := New(&fakeLLM{err: boom}, Options{}).Score(context.Background(), libraryScoreRequest()); !errors.Is(err, boom) {
		t.Fatalf("stream error: %v", err)
	}
	if _, err := New(&fakeLLM{events: []ai.Event{{Type: ai.EventCompleted}}}, Options{}).Score(context.Background(), libraryScoreRequest()); err == nil {
		t.Fatal("empty output accepted")
	}
	if _, err := New(nil, Options{}).Score(context.Background(), libraryScoreRequest()); !errors.Is(err, ErrNoLLM) {
		t.Fatalf("nil LLM: %v", err)
	}
	if _, _, err := New(nil, Options{}).Decide(context.Background(), decision.Request{Text: "x", Taxonomy: taxonomy()}); !errors.Is(err, ErrNoLLM) {
		t.Fatalf("nil LLM decide: %v", err)
	}
}

// A model's answer may arrive as text rather than a structured event, and with
// no usage event the usage is simply zero.
func TestScore_ParsesTextAndToleratesNoUsage(t *testing.T) {
	llm := &fakeLLM{events: []ai.Event{{Type: ai.EventTextDelta, Text: "```json\n" + goodScores + "\n```"}, {Type: ai.EventCompleted}}}
	res, err := New(llm, Options{}).Score(context.Background(), libraryScoreRequest())
	if err != nil || res.Usage != (decision.Usage{}) || res.Engine != "llm-decider" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A single candidate always normalises to 1.
func TestScore_OneCandidateChoiceIsCertain(t *testing.T) {
	req := decision.ScoreRequest{Text: "x", Questions: []decision.Question{
		{ID: "q", Kind: decision.KindChoice, Candidates: []decision.Candidate{{ID: "a"}}},
	}}
	llm := scoresLLM(t, `{"answers":[{"questionId":"q","scores":[{"id":"a","probability":0.2}]}]}`)
	res, err := New(llm, Options{}).Score(context.Background(), req)
	if err != nil || res.Answers["q"].Scores[0].Probability != 1 || decision.ValidateScoreResult(req, res) != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
