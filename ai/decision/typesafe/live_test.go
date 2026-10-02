package typesafe

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// Live integration test, opt-in: it calls the real TypeSafe API and is skipped
// unless BOTH JEV_API_KEY is set and AICHAT_LIVE_JEV=1. It makes two calls.
// The key is read from the environment here and goes nowhere but the
// Authorization header; the test logs probabilities, latency and token usage
// only.
//
//	AICHAT_LIVE_JEV=1 go test ./ai/decision/typesafe -run Live -v
func TestLive_ChinookTableRelevance(t *testing.T) {
	key := os.Getenv("JEV_API_KEY")
	if key == "" || os.Getenv("AICHAT_LIVE_JEV") != "1" {
		t.Skip("live Jev test: set JEV_API_KEY and AICHAT_LIVE_JEV=1 to run")
	}

	var last CallEvent
	c, err := New(Config{APIKey: key, Name: "jev", OnCall: func(e CallEvent) { last = e }})
	if err != nil {
		t.Fatal(err)
	}

	tables := []string{"Album", "Artist", "Customer", "Employee", "Genre", "Invoice", "InvoiceLine", "MediaType", "Playlist", "PlaylistTrack", "Track"}
	columns := map[string]string{
		"Album":         "AlbumId, Title, ArtistId",
		"Artist":        "ArtistId, Name",
		"Customer":      "CustomerId, FirstName, LastName, Company, Address, City, State, Country, PostalCode, Phone, Fax, Email, SupportRepId",
		"Employee":      "EmployeeId, LastName, FirstName, Title, ReportsTo, BirthDate, HireDate, Address, City, State, Country, PostalCode, Phone, Fax, Email",
		"Genre":         "GenreId, Name",
		"Invoice":       "InvoiceId, CustomerId, InvoiceDate, BillingAddress, BillingCity, BillingState, BillingCountry, BillingPostalCode, Total",
		"InvoiceLine":   "InvoiceLineId, InvoiceId, TrackId, UnitPrice, Quantity",
		"MediaType":     "MediaTypeId, Name",
		"Playlist":      "PlaylistId, Name",
		"PlaylistTrack": "PlaylistId, TrackId",
		"Track":         "TrackId, Name, AlbumId, MediaTypeId, GenreId, Composer, Milliseconds, Bytes, UnitPrice",
	}

	for _, withFields := range []bool{false, true} {
		name := "names only"
		if withFields {
			name = "with field names and a database description"
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
				dbContext = map[string]any{"database": "Chinook: a digital music store. Customers buy tracks; each purchase is an invoice with invoice lines."}
			}
			req := decision.ScoreRequest{
				Context: dbContext,
				Text:    "Which countries buy the most music relative to their population?",
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

// TestLive_Decide checks the taxonomy mapping of Decide end to end (one call).
func TestLive_Decide(t *testing.T) {
	key := os.Getenv("JEV_API_KEY")
	if key == "" || os.Getenv("AICHAT_LIVE_JEV") != "1" {
		t.Skip("live Jev test: set JEV_API_KEY and AICHAT_LIVE_JEV=1 to run")
	}
	var last CallEvent
	c, err := New(Config{APIKey: key, Name: "jev", OnCall: func(e CallEvent) { last = e }})
	if err != nil {
		t.Fatal(err)
	}
	req := decision.Request{
		Text: "Which countries buy the most music relative to their population?",
		Taxonomy: decision.Taxonomy{
			Modules: []decision.ModuleSpec{
				{Name: "investigate", Intents: []string{"aggregate", "relate", "explain_path"}},
				{Name: "settings", Intents: []string{"change"}},
			},
			Presentations: []string{"grid", "bar_chart", "single_value"},
			DataKinds:     []string{"population_per_country", "weather"},
			Descriptions: map[string]string{
				"investigate/aggregate":  "total or average of one measure per group",
				"investigate/relate":     "rank one entity by another's behaviour",
				"population_per_country": "inhabitants of each country",
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, ok, err := c.Decide(ctx, req)
	if err != nil {
		t.Fatalf("live call failed: %v", err)
	}
	t.Logf("ok=%v latency=%s tokens in=%d out=%d", ok, last.Latency.Round(time.Millisecond), last.Usage.InputTokens, last.Usage.OutputTokens)
	t.Logf("module=%s intent=%s conf=%.2f interaction=%s presentation=%q data=%v scopes=%v", d.Module.Value, d.Intent.Value, d.Intent.Confidence, d.Interaction, d.Presentation, d.RequiredData, d.RequiredScopes)
	pol := decision.NarrowingPolicy()
	t.Logf("policy outcome: %s", pol.EvaluateDecision(d).Outcome)
	for id, p := range d.Scores {
		t.Logf("  %-26s %.2f", id, p)
	}
}
