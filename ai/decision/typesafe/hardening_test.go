package typesafe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

func oneNoul() AskRequest {
	return AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}}
}

func TestNew_BaseURLMustBeHTTPSExceptForLoopback(t *testing.T) {
	ok := []string{
		"https://api.typesafe.ai", "https://example.com/proxy/", "http://127.0.0.1:8080", "http://localhost:9",
		"http://[::1]:80", "http://jev.localhost", "http://127.1.2.3",
	}
	for _, u := range ok {
		if _, err := New(Config{APIKey: testKey, Model: testModel, BaseURL: u}); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	bad := map[string]string{
		"http://api.example.com":              "must be https",
		"http://10.0.0.5":                     "must be https",
		"ftp://example.com":                   "must be https",
		"https://user:SECRET-PW@example.com/": "credentials",
		"https://":                            "not a valid URL",
		"://nonsense":                         "not a valid URL",
		"example.com":                         "not a valid URL",
	}
	for u, want := range bad {
		_, err := New(Config{APIKey: testKey, Model: testModel, BaseURL: u})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "SECRET-PW") {
			t.Errorf("%s: err = %v, want %q", u, err, want)
		}
	}
}

// A 307/308 would make a redirect-following client re-send the state and the
// bearer key to whatever host the first answers with.
func TestDefaultClientNeverFollowsRedirects(t *testing.T) {
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c, err := New(Config{APIKey: testKey, Model: testModel, BaseURL: redirector.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Ask(context.Background(), oneNoul())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTemporaryRedirect || !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect target received the request")
	}
}

func TestAsk_LocalChecksRefuseBeforeAnyCall(t *testing.T) {
	d := &fakeDoer{body: docSample}
	c := newClient(t, d)

	many := map[string]any{}
	for i := 0; i <= MaxChoiceOptions; i++ {
		many[fmt.Sprintf("secret-option-%d", i)] = nil
	}
	tooManyQuestions := map[string]Question{}
	for i := 0; i <= MaxQuestionsPerCall; i++ {
		tooManyQuestions[fmt.Sprintf("q%d", i)] = Noul("?")
	}
	cases := map[string]struct {
		req  AskRequest
		want error
	}{
		"options":              {AskRequest{State: "x", Questions: map[string]Question{"q": Choice("?", many)}}, ErrTooManyOptions},
		"questions":            {AskRequest{State: "x", Questions: tooManyQuestions}, ErrTooManyQuestions},
		"state":                {AskRequest{State: strings.Repeat("a", 4*DefaultMaxStateTokens+8), Questions: map[string]Question{"q": Noul("?")}}, ErrStateTooLarge},
		"question":             {AskRequest{State: "x", Questions: map[string]Question{"q": Noul(strings.Repeat("a", 4*DefaultMaxStateTokens))}}, ErrStateTooLarge},
		"unencodable state":    {AskRequest{State: make(chan int), Questions: map[string]Question{"q": Noul("?")}}, ErrInvalidRequest},
		"unencodable question": {AskRequest{State: "x", Questions: map[string]Question{"q": Noul(make(chan int))}}, ErrInvalidRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Ask(context.Background(), tc.req)
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, decision.ErrInvalidRequest) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "secret-option") || d.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, d.calls)
			}
		})
	}

	// Just under the budget goes through; a negative budget switches the check off.
	under := AskRequest{State: strings.Repeat("a", 4*DefaultMaxStateTokens-200), Questions: map[string]Question{"q": Noul("?")}}
	if _, err := c.Ask(context.Background(), under); err != nil {
		t.Fatalf("under the budget: %v", err)
	}
	huge := AskRequest{State: strings.Repeat("a", 8*DefaultMaxStateTokens), Questions: map[string]Question{"q": Noul("?")}}
	off := newClient(t, d, func(c *Config) { c.MaxStateTokens = -1 })
	if _, err := off.Ask(context.Background(), huge); err != nil {
		t.Fatalf("check not disabled: %v", err)
	}
	custom := newClient(t, d, func(c *Config) { c.MaxStateTokens = 10 })
	if _, err := custom.Ask(context.Background(), oneNoul()); err != nil {
		t.Fatalf("tiny request over a tiny budget: %v", err)
	}
	if _, err := custom.Ask(context.Background(), AskRequest{State: strings.Repeat("a", 100), Questions: oneNoul().Questions}); !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("custom budget ignored: %v", err)
	}
}

// 255 options is the API's limit: Decide enforces it as Score does.
func TestDecide_RefusesMoreThan255IntentOptions(t *testing.T) {
	var intents []string
	for i := 0; i < MaxChoiceOptions; i++ { // 255 intents + "other" = 256 options
		intents = append(intents, fmt.Sprintf("i%d", i))
	}
	req := decision.Request{Text: "x", Taxonomy: decision.Taxonomy{Modules: []decision.ModuleSpec{{Name: "m", Intents: intents}}}}
	d := &fakeDoer{body: docSample}
	if _, _, err := newClient(t, d).Decide(context.Background(), req); !errors.Is(err, ErrTooManyOptions) || d.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, d.calls)
	}
}

func TestErrors_CallerFaultsMatchTheEngineNeutralSentinels(t *testing.T) {
	for status, want := range map[int]error{401: decision.ErrAuth, 403: decision.ErrAuth, 400: decision.ErrInvalidRequest, 422: decision.ErrInvalidRequest} {
		_, err := newClient(t, &fakeDoer{status: status}).Ask(context.Background(), oneNoul())
		if !errors.Is(err, want) {
			t.Errorf("%d: %v", status, err)
		}
	}
	for _, status := range []int{429, 500, 529, 404} {
		_, err := newClient(t, &fakeDoer{status: status}).Ask(context.Background(), oneNoul())
		if errors.Is(err, decision.ErrAuth) || errors.Is(err, decision.ErrInvalidRequest) {
			t.Errorf("%d must count as an engine failure: %v", status, err)
		}
	}
}

func TestErrors_RetryAfterInBothFormsAndCapped(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	httpDate := func(d time.Duration) string { return base.Add(d).Format(http.TimeFormat) }
	cases := map[string]time.Duration{
		// seconds
		"7": 7 * time.Second, " 12 ": 12 * time.Second, "0": 0, "-5": 0, "864000": decision.MaxRetryDelay,
		// malformed, empty, and too large for an int
		"soon": 0, "": 0, "99999999999999999999": 0,
		// HTTP dates, relative to the clock: future, past, far future (capped)
		httpDate(90 * time.Second): 90 * time.Second, httpDate(-time.Hour): 0, httpDate(48 * time.Hour): decision.MaxRetryDelay,
	}
	for v, want := range cases {
		if got := parseRetryAfter(v, base); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}

	// End to end: the error carries it, and decision.RetryDelay reads it.
	d := &fakeDoer{status: 429, headers: map[string]string{"Retry-After": "7"}}
	_, err := newClient(t, d).Ask(context.Background(), oneNoul())
	if got := decision.RetryDelay(err); got != 7*time.Second {
		t.Fatalf("RetryDelay = %v", got)
	}
	d = &fakeDoer{status: 429, headers: map[string]string{"Retry-After": httpDate(30 * time.Second)}}
	c := newClient(t, d, func(c *Config) { c.Clock = func() time.Time { return base } })
	if _, err := c.Ask(context.Background(), oneNoul()); decision.RetryDelay(err) != 30*time.Second {
		t.Fatalf("date form: %v", err)
	}
}

func TestErrorsNeverQuoteCallerContent(t *testing.T) {
	const secret = "SECRET-CANDIDATE-ID"
	req := libraryRequest()
	req.Questions[1].Candidates[0].ID = secret // the choice question's first candidate

	// A response that answers with an option the request never had.
	body := `{"model":"m","answers":{` +
		`"q0.c0":{"type":"noul","noul":0.1},"q0.c1":{"type":"noul","noul":0.1},"q0.c2":{"type":"noul","noul":0.1},"q0.c3":{"type":"noul","noul":0.1},` +
		`"q1":{"type":"choice","choice":"x","confidence":0.5,"probabilities":{"x":1.0}}}}`
	_, err := newClient(t, &fakeDoer{body: body}).Score(context.Background(), req)
	if !errors.Is(err, ErrBadResponse) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "tables") || strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v", err)
	}

	// A request that fails local validation (duplicate candidate).
	req = libraryRequest()
	req.Questions[0].Candidates[1].ID = secret
	req.Questions[0].Candidates[0].ID = secret
	d := &fakeDoer{body: body}
	_, err = newClient(t, d).Score(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, decision.ErrInvalidRequest) || strings.Contains(err.Error(), secret) || d.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, d.calls)
	}

	// A malformed answer is named by position, not id.
	_, err = newClient(t, &fakeDoer{body: `{"model":"m","answers":{"q1":{"type":"noul"}}}`}).Score(context.Background(), libraryRequest())
	if !errors.Is(err, ErrBadResponse) || strings.Contains(err.Error(), "kind") || strings.Contains(err.Error(), "tables") {
		t.Fatalf("err = %v", err)
	}
	_, err = newClient(t, &fakeDoer{body: `{"model":"m","answers":{"q0.c0":{"type":"noul","noul":1}}}`}).Score(context.Background(), libraryRequest())
	if !errors.Is(err, ErrBadResponse) || strings.Contains(err.Error(), "tables") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecisionAndTraceRecordTheModelTheAPIReported(t *testing.T) {
	d := &fakeDoer{body: response(fullAnswers()...)}
	pol := decision.NarrowingPolicy()
	got, ok, tr := decision.Chain{Providers: []decision.Provider{newClient(t, d)}, Policy: &pol}.Decide(context.Background(), decideRequest())
	if !ok || got.Model != "jev-1.13.0" || tr.Model != "jev-1.13.0" {
		t.Fatalf("ok=%v model=%q trace model=%q", ok, got.Model, tr.Model)
	}
}
