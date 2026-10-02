package typesafe

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// Live integration tests, opt-in: they call the real TypeSafe API and are
// skipped unless BOTH JEV_API_KEY is set and AICHAT_LIVE_JEV=1. The score test
// makes two calls and the decide test one. The key is read from the environment
// here and goes nowhere but the Authorization header; the tests log
// probabilities, the model id, latency and token usage only. The model is
// JEV_MODEL when set, else ModelLatest (an explicit choice here: the point is
// to see what the alias serves today).
//
//	AICHAT_LIVE_JEV=1 JEV_MODEL=jev-1.13.0 go test ./ai/decision/typesafe -run Live -v
//
// The fixture is a small invented library schema; nothing in it is real data.
func liveClient(t *testing.T, last *CallEvent) *Client {
	t.Helper()
	key := os.Getenv("JEV_API_KEY")
	if key == "" || os.Getenv("AICHAT_LIVE_JEV") != "1" {
		t.Skip("live Jev test: set JEV_API_KEY and AICHAT_LIVE_JEV=1 to run")
	}
	model := os.Getenv("JEV_MODEL")
	if model == "" {
		model = ModelLatest
	}
	c, err := New(Config{APIKey: key, Model: model, Name: "jev", OnCall: func(e CallEvent) { *last = e }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const liveQuestion = "Which members borrowed the most books last month?"

func TestLive_TableRelevance(t *testing.T) {
	var last CallEvent
	c := liveClient(t, &last)

	tables := []string{"authors", "books", "loans", "members", "shelves"}
	columns := map[string]string{
		"authors": "author_id, name, birth_year",
		"books":   "book_id, title, author_id, published_year, shelf_id",
		"loans":   "loan_id, member_id, book_id, loaned_on, due_on, returned_on",
		"members": "member_id, name, email, joined_on",
		"shelves": "shelf_id, room, position",
	}

	for _, withFields := range []bool{false, true} {
		name := "names only"
		if withFields {
			name = "with field names and a description of the data"
		}
		t.Run(name, func(t *testing.T) {
			var cands []decision.Candidate
			for _, tb := range tables {
				cand := decision.Candidate{ID: tb}
				if withFields {
					cand.Description = "table " + tb + " with columns " + columns[tb]
				}
				cands = append(cands, cand)
			}
			choiceCands := append(append([]decision.Candidate(nil), cands...), decision.Candidate{ID: "none", Description: "none of these tables"})
			var dbContext map[string]any
			if withFields {
				dbContext = map[string]any{"database": "A small lending library: members borrow books; each borrowing is a loan."}
			}
			req := decision.ScoreRequest{
				Context: dbContext,
				Text:    liveQuestion,
				Questions: []decision.Question{
					{ID: "needed", Kind: decision.KindRelevance, Instructions: "Is this database table needed to answer the question?", Candidates: cands},
					{ID: "primary", Kind: decision.KindChoice, Instructions: "Which single database table is most needed to answer the question?", Candidates: choiceCands, NoneID: "none"},
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := c.Score(ctx, req)
			if err != nil {
				t.Fatalf("live call failed: %v", err)
			}
			if err := decision.ValidateScoreResult(req, res); err != nil {
				t.Fatalf("live answer does not validate: %v", err)
			}
			t.Logf("model=%s latency=%s input_tokens=%d output_tokens=%d", res.Model, last.Latency.Round(time.Millisecond), res.Usage.InputTokens, res.Usage.OutputTokens)
			pol := decision.NarrowingPolicy()
			for _, id := range []string{"needed", "primary"} {
				a := res.Answers[id]
				sel := pol.Evaluate(a)
				t.Logf("%s (confidence=%.2f calibrated=%v) -> %s picks=%v potential=%v reason=%s", id, a.Confidence, a.Calibrated, sel.Outcome, sel.Picks, sel.Potential, sel.Reason)
				for _, s := range a.Scores {
					t.Logf("  %-14s %.2f", s.ID, s.Probability)
				}
			}
		})
	}
}

// TestLive_Decide checks the taxonomy mapping of Decide end to end (one call),
// including the interaction question, which is always asked.
func TestLive_Decide(t *testing.T) {
	var last CallEvent
	c := liveClient(t, &last)
	req := decision.Request{
		Text: liveQuestion,
		Taxonomy: decision.Taxonomy{
			Modules: []decision.ModuleSpec{
				{Name: "investigate", Intents: []string{"aggregate", "relate", "explain_path"}},
				{Name: "settings", Intents: []string{"change"}},
			},
			Presentations: []string{"grid", "bar_chart", "single_value"},
			DataKinds:     []string{"membership_dates", "weather"},
			Descriptions: map[string]string{
				"investigate/aggregate": "total or average of one measure per group",
				"investigate/relate":    "rank one entity by another's behaviour",
				"membership_dates":      "the date each member joined",
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, ok, err := c.Decide(ctx, req)
	if err != nil {
		t.Fatalf("live call failed: %v", err)
	}
	t.Logf("ok=%v model=%s latency=%s tokens in=%d out=%d", ok, d.Model, last.Latency.Round(time.Millisecond), last.Usage.InputTokens, last.Usage.OutputTokens)
	t.Logf("module=%s intent=%s conf=%.2f interaction=%s presentation=%q data=%v scopes=%v", d.Module.Value, d.Intent.Value, d.Intent.Confidence, d.Interaction, d.Presentation, d.RequiredData, d.RequiredScopes)
	pol := decision.NarrowingPolicy()
	t.Logf("policy outcome: %s", pol.EvaluateDecision(d).Outcome)
	for id, p := range d.Scores {
		t.Logf("  %-26s %.2f", id, p)
	}
}
