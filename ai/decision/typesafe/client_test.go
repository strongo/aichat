package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// docSample is the documented example response (API reference, "Choice
// answer", "Score answer", "Noul answer" combined).
const docSample = `{
  "model": "jev-1.13.0",
  "answers": {
    "department": {"type": "choice", "choice": "technical", "confidence": 0.78,
      "probabilities": {"technical": 0.85, "sales": 0.0, "billing": 0.15}},
    "frustration": {"type": "score", "score": 1.0, "confidence": 1.0,
      "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
      "probabilities": {"0": 0.0, "1": 1.0, "2": 0.0}},
    "is_urgent": {"type": "noul", "noul": 1.0}
  },
  "usage": {"input_tokens": 392, "output_tokens": 65}
}`

func TestNew(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty key accepted")
	}
	c, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "typesafe" || c.DecisionTimeout() != DefaultTimeout {
		t.Fatalf("name=%q timeout=%v", c.Name(), c.DecisionTimeout())
	}
	c, _ = New(Config{APIKey: testKey, Name: "jev", Timeout: time.Second})
	if c.Name() != "jev" || c.DecisionTimeout() != time.Second {
		t.Fatalf("name=%q timeout=%v", c.Name(), c.DecisionTimeout())
	}
	if s := c.String(); strings.Contains(s, testKey) || !strings.Contains(s, DefaultModel) || !strings.Contains(s, DefaultBaseURL) {
		t.Fatalf("String() = %q", s)
	}
}

func TestAsk_RequestShapeAndDecodedResponse(t *testing.T) {
	d := &fakeDoer{body: docSample}
	var events []CallEvent
	c := newClient(t, d, func(c *Config) {
		c.Clock = steppingClock()
		c.OnCall = func(e CallEvent) { events = append(events, e) }
	})

	resp, err := c.Ask(context.Background(), AskRequest{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]Question{
			"department":  Choice("Which team should handle this?", map[string]any{"billing": "Payments", "technical": nil}),
			"frustration": Score("How frustrated?", "Calm", "Frustrated", "Very angry"),
			"is_urgent":   Noul("Does this convey urgency?"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := d.req
	if r.Method != http.MethodPost || r.URL.String() != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("%s %s", r.Method, r.URL)
	}
	if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", r.Header)
	}
	sent := d.sent(t)
	if sent["model"] != "jev-latest" || sent["state"] != "Help! My payouts have been failing for 3 days." {
		t.Fatalf("sent = %v", sent)
	}
	q := d.questions(t)
	if q["is_urgent"]["type"] != "noul" || q["is_urgent"]["instructions"] != "Does this convey urgency?" {
		t.Fatalf("noul = %v", q["is_urgent"])
	}
	if q["frustration"]["type"] != "score" || len(q["frustration"]["criteria"].([]any)) != 3 {
		t.Fatalf("score = %v", q["frustration"])
	}
	crit := q["department"]["criteria"].(map[string]any)
	if q["department"]["type"] != "choice" || crit["billing"] != "Payments" || crit["technical"] != nil {
		t.Fatalf("choice = %v", q["department"])
	}
	if _, ok := crit["technical"]; !ok {
		t.Fatal("an option with no description must still be sent (as null)")
	}

	if resp.Model != "jev-1.13.0" || resp.Usage.InputTokens != 392 || resp.Usage.OutputTokens != 65 {
		t.Fatalf("resp = %+v", resp)
	}
	dep := resp.Answers["department"]
	if dep.Choice != "technical" || dep.Probabilities["billing"] != 0.15 || dep.Confidence == nil || *dep.Confidence != 0.78 {
		t.Fatalf("department = %+v", dep)
	}
	fr := resp.Answers["frustration"]
	if fr.Score != 1.0 || fr.Legend["1"] != "Frustrated" {
		t.Fatalf("frustration = %+v", fr)
	}
	if u := resp.Answers["is_urgent"]; u.Noul != 1.0 || u.Confidence != nil {
		t.Fatalf("is_urgent = %+v", u)
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	e := events[0]
	if e.Engine != "typesafe" || e.Model != "jev-1.13.0" || e.Usage.InputTokens != 392 || e.Questions != 3 || e.Status != 200 || e.Err != nil || e.Latency != 10*time.Millisecond {
		t.Fatalf("event = %+v", e)
	}
}

func TestAsk_BaseURLModelAndTrailingSlash(t *testing.T) {
	d := &fakeDoer{body: docSample}
	c := newClient(t, d, func(c *Config) { c.BaseURL = "http://localhost:9/"; c.Model = "jev-1.13.0" })
	if _, err := c.Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}}); err != nil {
		t.Fatal(err)
	}
	if d.req.URL.String() != "http://localhost:9/v1/systemone" || d.sent(t)["model"] != "jev-1.13.0" {
		t.Fatalf("url=%s sent=%v", d.req.URL, d.sent(t))
	}
}

func TestAsk_HTTPErrorMapping(t *testing.T) {
	const echoed = "SECRET-STATE-ECHO"
	cases := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		is      error
		typ     string
		retry   time.Duration
	}{
		{"401 live shape", 401, `{"detail":{"error_type":"authentication_error","message":"` + echoed + `"}}`, map[string]string{"x-typesafe-request-id": "req_1"}, ErrAuth, "authentication_error", 0},
		{"403", 403, `{}`, nil, ErrAuth, "", 0},
		{"400 live shape (string detail)", 400, `{"detail":"Noul question must have criteria or instructions: ` + echoed + `"}`, nil, ErrInvalidRequest, "", 0},
		{"422 documented", 422, `{"detail":[{"loc":["body"],"input":"` + echoed + `"}]}`, nil, ErrInvalidRequest, "", 0},
		{"429", 429, `not json ` + echoed, map[string]string{"Retry-After": "7"}, ErrRateLimited, "", 7 * time.Second},
		{"429 bad retry-after", 429, ``, map[string]string{"Retry-After": "soon"}, ErrRateLimited, "", 0},
		{"429 negative retry-after", 429, ``, map[string]string{"Retry-After": "-3"}, ErrRateLimited, "", 0},
		{"529", 529, ``, nil, ErrOverloaded, "", 0},
		{"500", 500, ``, nil, ErrServer, "", 0},
		{"503", 503, ``, nil, ErrServer, "", 0},
		{"404", 404, ``, nil, ErrUnexpectedStatus, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDoer{status: tc.status, body: tc.body, headers: tc.headers}
			var ev CallEvent
			c := newClient(t, d, func(c *Config) { c.OnCall = func(e CallEvent) { ev = e } })
			_, err := c.Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}})
			var ae *APIError
			if !errors.As(err, &ae) || !errors.Is(err, tc.is) {
				t.Fatalf("err = %v (%T)", err, err)
			}
			if ae.Status != tc.status || ae.ErrorType != tc.typ || ae.RetryAfter != tc.retry {
				t.Fatalf("api error = %+v", ae)
			}
			if strings.Contains(err.Error(), echoed) || strings.Contains(err.Error(), testKey) {
				t.Fatalf("error leaks body or key: %v", err)
			}
			if ev.Status != tc.status || ev.Err == nil || ev.Model != "" {
				t.Fatalf("event = %+v", ev)
			}
			if d.calls != 1 {
				t.Fatalf("calls = %d: the provider must never retry", d.calls)
			}
		})
	}
}

func TestAPIError_ErrorString(t *testing.T) {
	d := &fakeDoer{status: 401, body: `{"detail":{"error_type":"authentication_error"}}`, headers: map[string]string{"x-typesafe-request-id": "req_abc"}}
	_, err := newClient(t, d).Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}})
	if err.Error() != "typesafe: HTTP 401 (authentication failed) authentication_error request req_abc" {
		t.Fatalf("%q", err)
	}
	d = &fakeDoer{status: 529}
	_, err = newClient(t, d).Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}})
	if err.Error() != "typesafe: HTTP 529 (overloaded)" {
		t.Fatalf("%q", err)
	}
}

func TestAsk_TransportAndContextErrors(t *testing.T) {
	ask := func(c *Client, ctx context.Context) error {
		_, err := c.Ask(ctx, AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}})
		return err
	}
	var ev CallEvent
	c := newClient(t, &fakeDoer{err: errNetwork}, func(c *Config) { c.OnCall = func(e CallEvent) { ev = e } })
	if err := ask(c, context.Background()); !errors.Is(err, errNetwork) || strings.Contains(err.Error(), testKey) || ev.Status != 0 || ev.Err == nil {
		t.Fatalf("err=%v ev=%+v", err, ev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ask(newClient(t, &fakeDoer{body: docSample}), ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestAsk_MalformedResponses(t *testing.T) {
	const echoed = "SECRET-STATE-ECHO"
	cases := map[string]*fakeDoer{
		"not json":     {body: `{"answers": ` + echoed},
		"no answers":   {body: `{"model":"jev-1.13.0"}`},
		"too large":    {body: docSample},
		"body unread":  {readErr: errNetwork},
		"empty object": {body: `[]`},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, d, func(c *Config) {
				if name == "too large" {
					c.MaxResponseBytes = 20
				}
			})
			_, err := c.Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}})
			if err == nil || strings.Contains(err.Error(), echoed) {
				t.Fatalf("err = %v", err)
			}
			if name != "body unread" && !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err = %v, want ErrBadResponse", err)
			}
			if name == "body unread" && !errors.Is(err, errNetwork) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAsk_RequestConstructionErrors(t *testing.T) {
	c := newClient(t, &fakeDoer{}, func(c *Config) { c.BaseURL = "http://[::1" })
	if _, err := c.Ask(context.Background(), AskRequest{State: "x", Questions: map[string]Question{"q": Noul("?")}}); err == nil {
		t.Fatal("bad base URL accepted")
	}
	d := &fakeDoer{}
	c = newClient(t, d)
	if _, err := c.Ask(context.Background(), AskRequest{State: make(chan int), Questions: map[string]Question{"q": Noul("?")}}); err == nil || d.calls != 0 {
		t.Fatalf("unencodable state: err=%v calls=%d", err, d.calls)
	}
}

func TestQuestionConstructorsEncode(t *testing.T) {
	b, _ := json.Marshal(map[string]Question{"s": Score("rate", "low", "high"), "n": Noul("yes?")})
	want := `{"n":{"type":"noul","instructions":"yes?"},"s":{"type":"score","instructions":"rate","criteria":["low","high"]}}`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
}
