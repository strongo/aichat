package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
)

func libraryScoreRequest() decision.ScoreRequest {
	return decision.ScoreRequest{
		Text: "Which members borrowed the most books last month?",
		Questions: []decision.Question{
			{ID: "needed", Kind: decision.KindRelevance, Instructions: "Is the table needed?", Candidates: []decision.Candidate{{ID: "loans"}, {ID: "members"}, {ID: "shelves"}}},
			{ID: "primary", Kind: decision.KindChoice, Instructions: "Which table?", NoneID: "none", Candidates: []decision.Candidate{{ID: "loans"}, {ID: "none"}}},
		},
	}
}

func goodScoreResponse() cloudproto.ScoreResponse {
	return cloudproto.ScoreResponse{
		Engine: "jev", Model: "jev-1.13.0", Strategy: "fallback", Calibrated: true,
		Attempts: []decision.Attempt{{Provider: "jev", Outcome: decision.AttemptDecided, Role: "primary", Latency: 120 * time.Millisecond}},
		Usage:    &ai.Usage{InputTokens: 300, OutputTokens: 60},
		Answers: map[string]decision.Answer{
			// deliberately unsorted: the client sorts
			"needed":  {Scores: []decision.Score{{ID: "shelves", Probability: 0.02}, {ID: "loans", Probability: 0.95}, {ID: "members", Probability: 0.7}}, Calibrated: true},
			"primary": {Scores: []decision.Score{{ID: "none", Probability: 0.1}, {ID: "loans", Probability: 0.9}}, Confidence: 0.8, HasConfidence: true, Calibrated: true, NoneID: "none"},
		},
	}
}

func scoreServer(t *testing.T, resp cloudproto.ScoreResponse, got *cloudproto.ScoreRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/"+cloudproto.PathScore {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(cloudproto.HeaderProduct) != "sneat" || r.Header.Get("Authorization") != "Bearer t" {
			t.Errorf("headers = %v", r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		if got != nil {
			_ = json.Unmarshal(b, got)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func scorer(t *testing.T, srvURL string) decision.TracedScorer {
	t.Helper()
	c := New(Config{BaseURL: srvURL + "/v0/", Product: "sneat", Token: tokenFunc("t"), ClientContext: &ai.ClientContext{}})
	ts, ok := c.Decider().(decision.TracedScorer)
	if !ok {
		t.Fatal("the cloud decider must be a TracedScorer (and so a ScoredProvider)")
	}
	return ts
}

func TestScore_PostsToAIScoreAndFoldsTheResponse(t *testing.T) {
	var got cloudproto.ScoreRequest
	srv := scoreServer(t, goodScoreResponse(), &got)
	defer srv.Close()
	p := scorer(t, srv.URL)

	ctx := WithInteractionID(context.Background(), "turn-7")
	req := libraryScoreRequest()
	res, rep, err := p.ScoreTraced(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// What was sent: the request, the product, the correlation fields.
	if got.Product != "sneat" || got.Text != req.Text || len(got.Questions) != 2 || got.InteractionID != "turn-7" || got.ClientContext == nil {
		t.Fatalf("sent = %+v", got)
	}

	// What came back.
	if res.Engine != "jev" || res.Model != "jev-1.13.0" || res.Usage != (decision.Usage{InputTokens: 300, OutputTokens: 60}) {
		t.Fatalf("result = %+v", res)
	}
	if err := decision.ValidateScoreResult(req, res); err != nil {
		t.Fatal(err)
	}
	needed := res.Answers["needed"]
	if needed.QuestionID != "needed" || needed.Kind != decision.KindRelevance || !needed.Calibrated || needed.Scores[0].ID != "loans" || needed.Scores[1].ID != "members" {
		t.Fatalf("needed = %+v", needed)
	}
	primary := res.Answers["primary"]
	if !primary.HasConfidence || primary.Confidence != 0.8 || primary.NoneID != "none" || primary.Kind != decision.KindChoice {
		t.Fatalf("primary = %+v", primary)
	}
	if rep.Strategy != "fallback" || rep.Engine != "jev" || rep.Model != "jev-1.13.0" || len(rep.Attempts) != 1 || rep.Attempts[0].Role != "primary" {
		t.Fatalf("report = %+v", rep)
	}
	// Score (untraced) is the same call.
	if res2, err := p.Score(ctx, req); err != nil || res2.Engine != "jev" {
		t.Fatalf("Score: %+v %v", res2, err)
	}
}

// A hosted fallback to an LLM must stay uncalibrated, even if a sloppy server
// leaves the per-answer flag on.
func TestScore_CalibratedNeedsBothTheAnswerAndTheResponse(t *testing.T) {
	resp := goodScoreResponse()
	resp.Calibrated, resp.Engine = false, ""
	srv := scoreServer(t, resp, nil)
	defer srv.Close()
	res, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "cloud-decision" {
		t.Fatalf("engine = %q", res.Engine)
	}
	for id, a := range res.Answers {
		if a.Calibrated {
			t.Fatalf("%s is calibrated although the response says it is not", id)
		}
	}
	sel := decision.NarrowingPolicy().Evaluate(res.Answers["needed"])
	if sel.Outcome != decision.OutcomeUnscored || len(sel.Picks) != 0 || len(sel.Proposals) == 0 {
		t.Fatalf("selection = %+v", sel)
	}
}

// oldServer is a server of this protocol that predates ai/score: ai/usage works,
// ai/score answers scoreStatus with scoreBody. Calls are counted per path.
func oldServer(t *testing.T, scoreStatus int, scoreBody string, scoreCalls, usageCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/" + cloudproto.PathUsage:
			usageCalls.Add(1)
			_, _ = w.Write([]byte(`{"product":"sneat"}`))
		case "/v0/" + cloudproto.PathScore:
			scoreCalls.Add(1)
			w.WriteHeader(scoreStatus)
			_, _ = w.Write([]byte(scoreBody))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
}

// New client, old server: no ai/score route. The engine is unsupported (and so
// skipped by a combinator, neutral for a breaker), never an error to retry. A 501
// says so by itself; a 404 or 405 is ambiguous and is settled by one GET
// ai/usage showing that the base URL does speak the protocol.
func TestScore_ServerWithoutTheRouteIsUnsupported(t *testing.T) {
	for status, wantUsage := range map[int]int32{http.StatusNotFound: 1, http.StatusMethodNotAllowed: 1, http.StatusNotImplemented: 0} {
		var score, usage atomic.Int32
		srv := oldServer(t, status, "<html>no such route</html>", &score, &usage)
		_, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
		srv.Close()
		if !errors.Is(err, decision.ErrUnsupported) || score.Load() != 1 || usage.Load() != wantUsage {
			t.Fatalf("%d: err=%v score calls=%d usage calls=%d (an unsupported route must not be retried)", status, err, score.Load(), usage.Load())
		}
	}
}

func TestScore_UnsupportedServerFailsOverInACombinatorAndLeavesTheBreakerAlone(t *testing.T) {
	var score, usage atomic.Int32
	srv := oldServer(t, http.StatusNotFound, "404 page not found", &score, &usage)
	defer srv.Close()
	hosted := compose.NewBreaker(New(Config{BaseURL: srv.URL + "/v0/", Product: "sneat", Token: tokenFunc("t")}).Decider(), compose.WithBreakerThreshold(1))
	backup := stubScorer{name: "local"}
	res, rep, err := compose.Fallback(hosted, backup).ScoreTraced(context.Background(), libraryScoreRequest())
	if err != nil || res.Engine != "local" || !rep.FallbackFired {
		t.Fatalf("res=%+v rep=%+v err=%v", res, rep, err)
	}
	if got := rep.Attempts[0].Outcome; got != decision.AttemptUnsupported {
		t.Fatalf("attempt = %s", got)
	}
	if hosted.State() != compose.BreakerClosed {
		t.Fatalf("an old server opened the breaker: %v", hosted.State())
	}
}

type stubScorer struct{ name string }

func (s stubScorer) Name() string { return s.name }
func (s stubScorer) Decide(context.Context, decision.Request) (decision.Decision, bool, error) {
	return decision.Decision{}, false, nil
}
func (s stubScorer) Score(_ context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	res := decision.ScoreResult{Engine: s.name, Answers: map[string]decision.Answer{}}
	for _, q := range req.Questions {
		var scores []decision.Score
		for i, c := range q.Candidates {
			scores = append(scores, decision.Score{ID: c.ID, Probability: map[bool]float64{true: 1, false: 0}[i == 0]})
		}
		res.Answers[q.ID] = decision.NewAnswer(q.ID, q.Kind, scores)
	}
	return res, nil
}

func TestScore_RequestRefusedLocallyMakesNoCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	p := scorer(t, srv.URL)
	bad := libraryScoreRequest()
	bad.Questions[0].Candidates[1].ID = "dup-secret"
	bad.Questions[0].Candidates[0].ID = "dup-secret"
	_, err := p.Score(context.Background(), bad)
	if !errors.Is(err, decision.ErrInvalidRequest) || strings.Contains(err.Error(), "dup-secret") || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	unencodable := libraryScoreRequest()
	unencodable.Context = map[string]any{"x": make(chan int)}
	if _, err := p.Score(context.Background(), unencodable); !errors.Is(err, decision.ErrInvalidRequest) || calls.Load() != 0 {
		t.Fatalf("unencodable: err=%v calls=%d", err, calls.Load())
	}
}

func TestScore_HTTPErrorsAndTransportFailures(t *testing.T) {
	// An application error on the route is an *ai.Error, retried like a decision.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream","message":"engines down"}}`))
	}))
	defer srv.Close()
	_, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != "upstream" || calls.Load() != retryAttempts() {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}

	// A transport failure is a retryable upstream error; a cancelled context is cancelled.
	c := New(Config{BaseURL: "https://unused.example/", Product: "sneat", Token: tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") })}})
	if _, err := c.Decider().(decision.ScoredProvider).Score(canceled(), libraryScoreRequest()); !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("cancelled: %v", err)
	}
	// Token failures are auth errors, not retried.
	c = New(Config{BaseURL: "https://unused.example/", Product: "sneat", Token: func(context.Context) (string, error) { return "", errors.New("no token") }})
	if _, err := c.Decider().(decision.ScoredProvider).Score(context.Background(), libraryScoreRequest()); !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeAuth {
		t.Fatalf("token: %v", err)
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// retryAttempts is how many times a retryable failure is tried (the shared
// retry helper's default).
func retryAttempts() int32 { return 3 }

func TestScore_MalformedAndInvalidResponses(t *testing.T) {
	serve := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	}
	// Not JSON: the error does not quote the body.
	srv := serve(`{"answers": SECRET-BODY`)
	_, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
	srv.Close()
	if err == nil || strings.Contains(err.Error(), "SECRET-BODY") || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v", err)
	}
	// Missing an answer, an unknown candidate, a probability out of range: invalid, content-free.
	for name, mutate := range map[string]func(*cloudproto.ScoreResponse){
		"missing answer": func(r *cloudproto.ScoreResponse) { delete(r.Answers, "primary") },
		"unknown candidate": func(r *cloudproto.ScoreResponse) {
			r.Answers["needed"] = decision.Answer{Scores: []decision.Score{{ID: "SECRET-ID", Probability: 1}}}
		},
		"out of range": func(r *cloudproto.ScoreResponse) {
			r.Answers["needed"] = decision.Answer{Scores: []decision.Score{{ID: "loans", Probability: 7}}}
		},
	} {
		resp := goodScoreResponse()
		mutate(&resp)
		srv := scoreServer(t, resp, nil)
		_, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
		srv.Close()
		if err == nil || strings.Contains(err.Error(), "SECRET-ID") || !strings.Contains(err.Error(), "invalid result") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// No usage is fine.
	resp := goodScoreResponse()
	resp.Usage = nil
	srv = scoreServer(t, resp, nil)
	defer srv.Close()
	res, err := scorer(t, srv.URL).Score(context.Background(), libraryScoreRequest())
	if err != nil || res.Usage != (decision.Usage{}) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(res.Answers["needed"].Scores[0].ID, "loans") {
		t.Fatal("unsorted answers must be sorted")
	}
}
