package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
)

func tokenFunc(tok string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return tok, nil }
}

func TestNew_NormalisesBaseURL(t *testing.T) {
	c := New(Config{BaseURL: "https://api.example.com/v0", Product: "sneat", Token: tokenFunc("t")})
	if c.cfg.BaseURL != "https://api.example.com/v0/" {
		t.Errorf("BaseURL = %q, want trailing slash", c.cfg.BaseURL)
	}
	c2 := New(Config{BaseURL: "https://api.example.com/v0/", Product: "sneat", Token: tokenFunc("t")})
	if c2.cfg.BaseURL != "https://api.example.com/v0/" {
		t.Errorf("BaseURL = %q", c2.cfg.BaseURL)
	}
}

func TestNew_Panics(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn()
		})
	}
	mustPanic("no base url", func() { New(Config{Token: tokenFunc("t")}) })
	mustPanic("no token", func() { New(Config{BaseURL: "https://x/"}) })
}

func TestStream_ChatOverSSE(t *testing.T) {
	var gotPath, gotAuth, gotProduct string
	var gotReq ai.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotProduct = r.Header.Get(cloudproto.HeaderProduct)
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotReq)
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventStarted, Provider: "cloud", Model: "auto"})
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventTextDelta, Text: "hi"})
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventCompleted})
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL + "/v0/", Product: "sneat", Token: tokenFunc("secret")})
	if c.Name() != "cloud" {
		t.Fatalf("Name() = %q", c.Name())
	}
	text, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
	if gotPath != "/v0/"+cloudproto.PathChat {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotProduct != "sneat" {
		t.Errorf("X-AI-Product = %q", gotProduct)
	}
	if gotReq.Product != "sneat" {
		t.Errorf("req.Product = %q, want default filled in from Config", gotReq.Product)
	}
}

func TestStream_HTTPErrorDecodesErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		b, _ := json.Marshal(cloudproto.ErrorResponse{Error: ai.Error{Code: ai.ErrCodeRateLimited, Message: "slow down", Retryable: true}})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeRateLimited || !aiErr.Retryable {
		t.Fatalf("err = %v", err)
	}
}

func TestDecide_Decided(t *testing.T) {
	var gotReq decision.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+cloudproto.PathDecision {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotReq)
		resp := cloudproto.DecisionResponse{
			Decided: true,
			Decision: decision.Decision{
				Module: decision.Scored{Value: "calendar", Confidence: 0.9},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	p := c.Decider()
	if p.Name() != "cloud-decision" {
		t.Fatalf("Name() = %q", p.Name())
	}
	d, ok, err := p.Decide(context.Background(), decision.Request{Text: "show my calendar"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !ok || d.Module.Value != "calendar" {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
	if gotReq.Text != "show my calendar" || gotReq.Product != "sneat" {
		t.Errorf("gotReq = %+v", gotReq)
	}
}

func TestDecide_Abstain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cloudproto.DecisionResponse{Decided: false})
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, ok, err := c.Decider().Decide(context.Background(), decision.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if ok {
		t.Fatal("expected abstention when decided=false")
	}
}

func TestDecider_HonoursDecisionTimeoutInterface(t *testing.T) {
	c := New(Config{BaseURL: "https://x/", Product: "sneat", Token: tokenFunc("t")})
	p := c.Decider()
	dt, ok := p.(interface{ DecisionTimeout() time.Duration })
	if !ok {
		t.Fatal("cloud decider must implement DecisionTimeout() time.Duration")
	}
	if dt.DecisionTimeout() != 4*time.Second {
		t.Errorf("DecisionTimeout() = %v, want 4s", dt.DecisionTimeout())
	}
}

func TestUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/"+cloudproto.PathUsage {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(cloudproto.UsageResponse{
			Product:   "sneat",
			Allowance: &ai.Allowance{Unit: "tokens", Used: 100, Limit: 1000},
		})
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	ur, err := c.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if ur.Product != "sneat" || ur.Allowance == nil || ur.Allowance.Used != 100 {
		t.Errorf("ur = %+v", ur)
	}
}

func TestNewRequest_TokenError(t *testing.T) {
	wantErr := errors.New("no token")
	c := New(Config{BaseURL: "https://x/", Product: "sneat", Token: func(context.Context) (string, error) {
		return "", wantErr
	}})
	_, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || !strings.Contains(aiErr.Message, wantErr.Error()) {
		t.Fatalf("err = %v, want an *ai.Error mentioning %v", err, wantErr)
	}
	// n2: a token-source failure is an auth problem, never retryable --
	// retrying without fixing whatever broke the token source just fails
	// the same way again.
	if aiErr.Code != ai.ErrCodeAuth {
		t.Errorf("Code = %q, want %q", aiErr.Code, ai.ErrCodeAuth)
	}
	if aiErr.Retryable {
		t.Error("a token-source error must not be retryable")
	}
}

func TestStream_MidStreamCancelIsCanceled(t *testing.T) {
	started := make(chan struct{})
	disconnected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventTextDelta, Text: "partial"})
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // server observes the client disconnect
		close(disconnected)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, _, err := ai.Collect(c.Stream(ctx, ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ErrCodeCanceled", err)
	}
	if aiErr.Retryable {
		t.Error("a cancellation must never be retryable")
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the client disconnect")
	}
}

// TestStream_TransportErrorAlreadyCanceledIsCanceled mirrors
// TestDecide_TransportErrorAlreadyCanceledIsCanceled for the Stream path:
// doStreamRequest's own ctx.Err()!=nil branch (cloud.go around line 153)
// used to be covered only incidentally by timing-sensitive
// mid-stream-cancel tests, which missed it under -race often enough to be
// nondeterministic. An already-cancelled ctx plus a transport that always
// fails hits that branch on the very first (and only, since the resulting
// *ai.Error is not retryable) attempt -- no goroutine, no timing.
func TestStream_TransportErrorAlreadyCanceledIsCanceled(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: connection aborted")
		})},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := ai.Collect(c.Stream(ctx, ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeCanceled}", err)
	}
	if aiErr.Retryable {
		t.Error("a cancellation must never be retryable")
	}
}

func TestDecodeHTTPError_429QuotaNotRetryable(t *testing.T) {
	// n3: a 429 whose body names ErrCodeQuota (allowance exhausted) must
	// NOT be forced retryable just because the status is 429 -- retrying an
	// exhausted quota fails the same way again.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"quota","message":"allowance exhausted"}}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeQuota {
		t.Fatalf("err = %v, want ErrCodeQuota", err)
	}
	if aiErr.Retryable {
		t.Error("a 429 quota error must not be forced retryable")
	}
}

func TestDecodeHTTPError_StatusMapping(t *testing.T) {
	// n11: 404/409/422 map to ErrCodeInvalid (a client-request problem, not
	// something retrying fixes).
	cases := []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity}
	for _, status := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`boom`))
		}))
		_, _, _, err := ai.Collect(New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Stream(context.Background(), ai.ChatRequest{}))
		srv.Close()
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("status %d: err = %v, want *ai.Error", status, err)
		}
		if aiErr.Code != ai.ErrCodeInvalid {
			t.Errorf("status %d: Code = %q, want %q", status, aiErr.Code, ai.ErrCodeInvalid)
		}
		if aiErr.Retryable {
			t.Errorf("status %d: must not be retryable", status)
		}
	}
}

func TestStream_FatalPairShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream","message":"down"}}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	var lastEvent ai.Event
	var lastErr error
	n := 0
	for ev, err := range c.Stream(context.Background(), ai.ChatRequest{}) {
		n++
		lastEvent, lastErr = ev, err
		if err != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected a fatal error")
	}
	if lastEvent.Type != ai.EventError || lastEvent.Error == nil {
		t.Fatalf("fatal yield must carry Event{Type: EventError, Error: e}, got %+v", lastEvent)
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1 (no EventStarted since the request never succeeded)", n)
	}
}

// roundTripFunc lets a test build an *http.Response/error directly, without
// a real network round trip.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStream_MarshalErrorFromInvalidResponseSchema(t *testing.T) {
	c := New(Config{BaseURL: "https://unused.example/", Product: "sneat", Token: tokenFunc("t")})
	req := ai.ChatRequest{ResponseSchema: json.RawMessage(`{not valid`)}
	_, _, _, err := ai.Collect(c.Stream(context.Background(), req))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeInvalid {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeInvalid} from the failed json.Marshal", err)
	}
}

func TestDoStreamRequest_TransportErrorNotCanceledIsRetryableUpstream(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: dial failed")
		})},
	})
	_, err := c.doStreamRequest(context.Background(), []byte(`{}`))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !aiErr.Retryable {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeUpstream, Retryable: true}", err)
	}
}

// TestStream_LoopBodyCancelAfterFirstEventStillMarksCanceled deterministically
// hits the mid-loop "if err != nil && ctx.Err() != nil" remap (distinct from
// TestStream_MidStreamCancelIsCanceled, which is racy about whether
// cancellation lands before or after the response headers are received): it
// cancels from INSIDE the range body, right after the first successfully
// yielded event, guaranteeing the loop was already entered and blocked on
// the next read when the connection breaks.
func TestStream_LoopBodyCancelAfterFirstEventStillMarksCanceled(t *testing.T) {
	disconnected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventTextDelta, Text: "partial"})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(disconnected)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lastErr error
	var n int
	for ev, err := range c.Stream(ctx, ai.ChatRequest{}) {
		n++
		lastErr = err
		if err != nil {
			break
		}
		if ev.Type == ai.EventTextDelta {
			cancel()
		}
	}
	if n < 2 {
		t.Fatalf("n = %d, want at least 2 events (the text delta, then the fatal cancel)", n)
	}
	var aiErr *ai.Error
	if !errors.As(lastErr, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("lastErr = %v, want ai.Error{Code: ErrCodeCanceled}", lastErr)
	}
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never observed the client disconnect")
	}
}

// TestStream_FatalReadEventsErrorWithoutCancelStopsWhenConsumerStops covers
// the "if !yield(ev, err) { return }" branch for a fatal ReadEvents error
// that is NOT a ctx cancellation (a malformed event mid-stream): a
// well-behaved consumer (ai.Collect, which stops on the first error) must
// make Stream return right there.
func TestStream_FatalReadEventsErrorWithoutCancelStopsWhenConsumerStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		// A malformed JSON event body mid-stream: ReadEvents treats this as
		// a fatal (non-cancellation) error.
		_, _ = w.Write([]byte("event: response.started\ndata: {not valid json\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{}))
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeUpstream} for the malformed event", err)
	}
}

// TestStream_ConsumerContinuingPastFatalErrorStillReturns covers the final
// "if err != nil { return }" branch: even a consumer that returns true from
// yield despite seeing a non-nil err (choosing to keep "iterating") must not
// cause Stream to call cloudproto.ReadEvents again after its terminal event
// -- Stream itself stops the underlying range once it has seen one error,
// regardless of what the caller's callback returns. Invoking the iterator
// function directly (bypassing "for range" sugar) is the only way to
// control the returned bool independently of whether err is nil.
func TestStream_ConsumerContinuingPastFatalErrorStillReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.started\ndata: {not valid json\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	seq := c.Stream(context.Background(), ai.ChatRequest{})
	var calls int
	seq(func(ev ai.Event, err error) bool {
		calls++
		return true // always ask to keep going, even past a fatal error
	})
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1: Stream must stop after its own terminal event regardless of the callback's return value", calls)
	}
}

func TestDecide_MarshalErrorFromOutOfRangeTime(t *testing.T) {
	// time.Time.MarshalJSON itself errors for a year outside [0,9999] --
	// this is a genuine (if obscure) way json.Marshal(req) can fail for a
	// decision.Request, with no seam needed.
	c := New(Config{BaseURL: "https://unused.example/", Product: "sneat", Token: tokenFunc("t")})
	_, _, err := c.decide(context.Background(), decision.Request{Now: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("expected a json.Marshal error from an out-of-range time.Time")
	}
}

func TestDecide_NewRequestErrorFromInvalidBaseURL(t *testing.T) {
	// A control character in BaseURL makes http.NewRequestWithContext itself
	// fail, inside decide's retry closure.
	c := New(Config{BaseURL: "https://x/\x7f", Product: "sneat", Token: tokenFunc("t")})
	_, _, err := c.decide(context.Background(), decision.Request{})
	if err == nil {
		t.Fatal("expected an error building the request from an invalid BaseURL")
	}
}

func TestDecide_TransportErrorNotCanceledIsRetryableUpstream(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: dial failed")
		})},
	})
	_, _, err := c.decide(context.Background(), decision.Request{})
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !aiErr.Retryable {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeUpstream, Retryable: true}", err)
	}
}

func TestDecide_TransportErrorAlreadyCanceledIsCanceled(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: connection aborted")
		})},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := c.decide(ctx, decision.Request{})
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeCanceled}", err)
	}
}

func TestDecide_HTTPErrorStatusDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream","message":"decision service down"}}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, err := c.decide(context.Background(), decision.Request{})
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || !strings.Contains(aiErr.Message, "decision service down") {
		t.Fatalf("err = %v, want the decoded decision-service error", err)
	}
}

func TestDecide_DecodeResponseErrorOnInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, err := c.decide(context.Background(), decision.Request{})
	if err == nil || !strings.Contains(err.Error(), "cloud: decode decision response") {
		t.Fatalf("err = %v, want a decode-decision-response error", err)
	}
}

func TestUsage_TransportErrorNotCanceledIsRetryableUpstream(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: dial failed")
		})},
	})
	_, err := c.Usage(context.Background())
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeUpstream || !aiErr.Retryable {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeUpstream, Retryable: true}", err)
	}
}

func TestUsage_TransportErrorAlreadyCanceledIsCanceled(t *testing.T) {
	c := New(Config{
		BaseURL: "https://unused.example/",
		Product: "sneat",
		Token:   tokenFunc("t"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom: connection aborted")
		})},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Usage(ctx)
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeCanceled {
		t.Fatalf("err = %v, want ai.Error{Code: ErrCodeCanceled}", err)
	}
}

func TestUsage_HTTPErrorStatusDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream","message":"usage service down","retryable":true}}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, err := c.Usage(context.Background())
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || !strings.Contains(aiErr.Message, "usage service down") {
		t.Fatalf("err = %v, want the decoded usage-service error", err)
	}
}

func TestUsage_DecodeResponseErrorOnInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, err := c.Usage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cloud: decode usage response") {
		t.Fatalf("err = %v, want a decode-usage-response error", err)
	}
}

func TestUsage_NewRequestErrorFromInvalidBaseURL(t *testing.T) {
	c := New(Config{BaseURL: "https://x/\x7f", Product: "sneat", Token: tokenFunc("t")})
	_, err := c.Usage(context.Background())
	if err == nil {
		t.Fatal("expected an error building the request from an invalid BaseURL")
	}
}

func TestDecodeHTTPError_EmptyBodyFallsBackToHTTPStatusMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		// deliberately empty body: neither valid JSON nor plain text.
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) {
		t.Fatalf("err = %v, want *ai.Error", err)
	}
	if aiErr.Message != "HTTP 502" {
		t.Errorf("Message = %q, want the HTTP-status fallback \"HTTP 502\"", aiErr.Message)
	}
}

// TestDecodeHTTPError_HTMLBodyCollapsesToShortMessage covers the bug this
// guards against: an intermediary (e.g. Cloudflare) in front of the product
// API returning its own HTML error page for a 502/504 instead of relaying
// the application's JSON error. Before this fix, that raw HTML ended up
// verbatim in *ai.Error.Message (and from there in a chat transcript as
// "upstream: <!DOCTYPE html>...").
func TestDecodeHTTPError_HTMLBodyCollapsesToShortMessage(t *testing.T) {
	t.Run("text/html content-type", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>502 Bad Gateway</title></head><body>cloudflare</body></html>"))
		}))
		defer srv.Close()
		_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("err = %v, want *ai.Error", err)
		}
		want := "AI service unavailable (HTTP 502 Bad Gateway) -- try again later"
		if aiErr.Message != want {
			t.Errorf("Message = %q, want %q", aiErr.Message, want)
		}
		if strings.Contains(aiErr.Message, "<") || strings.Contains(aiErr.Message, "DOCTYPE") {
			t.Errorf("Message = %q, must never contain raw HTML", aiErr.Message)
		}
		if aiErr.Code != ai.ErrCodeUpstream {
			t.Errorf("Code = %q, want %q", aiErr.Code, ai.ErrCodeUpstream)
		}
		if !aiErr.Retryable {
			t.Error("Retryable = false, want true for a 502")
		}
	})

	t.Run("non-html content-type but body starts with '<'", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Deliberately a non-HTML Content-Type -- some intermediaries
			// mislabel (or omit) the header even while sending HTML, so the
			// leading '<' must be caught on its own.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("  <html>mislabeled content-type</html>"))
		}))
		defer srv.Close()
		_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("err = %v, want *ai.Error", err)
		}
		want := "AI service unavailable (HTTP 503 Service Unavailable) -- try again later"
		if aiErr.Message != want {
			t.Errorf("Message = %q, want %q", aiErr.Message, want)
		}
	})
}

// TestDecodeHTTPError_PlainTextBodyIsCapped covers the non-HTML, non-JSON
// fallback path: a short plain-text body passes through unchanged, but a
// long one is capped so an unexpectedly huge body from a misbehaving
// intermediary can never flow unbounded into a transcript or log line.
func TestDecodeHTTPError_PlainTextBodyIsCapped(t *testing.T) {
	t.Run("short body passes through", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("  upstream connection refused  "))
		}))
		defer srv.Close()
		_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("err = %v, want *ai.Error", err)
		}
		if aiErr.Message != "upstream connection refused" {
			t.Errorf("Message = %q, want the trimmed body unchanged", aiErr.Message)
		}
	})

	t.Run("long body is truncated with an ellipsis", func(t *testing.T) {
		long := strings.Repeat("x", maxNonJSONErrorExcerpt+50)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(long))
		}))
		defer srv.Close()
		_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) {
			t.Fatalf("err = %v, want *ai.Error", err)
		}
		wantPrefix := strings.Repeat("x", maxNonJSONErrorExcerpt)
		if !strings.HasPrefix(aiErr.Message, wantPrefix) {
			t.Errorf("Message does not start with the expected %d-char excerpt", maxNonJSONErrorExcerpt)
		}
		if !strings.HasSuffix(aiErr.Message, "…") {
			t.Errorf("Message = %q, want an ellipsis suffix marking truncation", aiErr.Message)
		}
		if len(aiErr.Message) >= len(long) {
			t.Errorf("Message length = %d, want it shorter than the original %d-byte body", len(aiErr.Message), len(long))
		}
	})
}

func TestNewRequestMethod_InvalidBaseURLErrors(t *testing.T) {
	c := New(Config{BaseURL: "https://x/\x7f", Product: "sneat", Token: tokenFunc("t")})
	_, err := c.newRequestMethod(context.Background(), http.MethodGet, cloudproto.PathUsage, nil)
	if err == nil {
		t.Fatal("expected an error building a request from an invalid BaseURL")
	}
}

func TestToAIError_AllBranches(t *testing.T) {
	t.Run("ai.Error with canceled ctx is remapped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		in := &ai.Error{Code: ai.ErrCodeUpstream, Message: "orig", Retryable: true}
		got := toAIError(ctx, in)
		if got.Code != ai.ErrCodeCanceled {
			t.Errorf("Code = %q, want %q", got.Code, ai.ErrCodeCanceled)
		}
	})
	t.Run("ai.Error with live ctx passes through unchanged", func(t *testing.T) {
		in := &ai.Error{Code: ai.ErrCodeQuota, Message: "orig"}
		got := toAIError(context.Background(), in)
		if got != in {
			t.Errorf("got = %+v, want the exact same *ai.Error returned unchanged", got)
		}
	})
	t.Run("bare context.Canceled becomes ErrCodeCanceled", func(t *testing.T) {
		got := toAIError(context.Background(), context.Canceled)
		if got.Code != ai.ErrCodeCanceled || got.Retryable {
			t.Errorf("got = %+v, want ErrCodeCanceled/not retryable", got)
		}
	})
	t.Run("bare context.DeadlineExceeded becomes ErrCodeCanceled", func(t *testing.T) {
		got := toAIError(context.Background(), context.DeadlineExceeded)
		if got.Code != ai.ErrCodeCanceled {
			t.Errorf("got = %+v, want ErrCodeCanceled", got)
		}
	})
	t.Run("unrelated error falls back to retryable upstream", func(t *testing.T) {
		got := toAIError(context.Background(), errors.New("boom"))
		if got.Code != ai.ErrCodeUpstream || !got.Retryable {
			t.Errorf("got = %+v, want ErrCodeUpstream/retryable", got)
		}
	})
}

func TestDecodeHTTPError_AuthStatusMapping(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`boom`))
		}))
		_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
		srv.Close()
		var aiErr *ai.Error
		if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeAuth {
			t.Errorf("status %d: err = %v, want ErrCodeAuth", status, err)
		}
	}
}

func TestDecodeHTTPError_RateLimitedStatusMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`slow down`))
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")}).Usage(context.Background())
	var aiErr *ai.Error
	if !errors.As(err, &aiErr) || aiErr.Code != ai.ErrCodeRateLimited || !aiErr.Retryable {
		t.Fatalf("err = %v, want ErrCodeRateLimited/retryable", err)
	}
}

func TestStream_Retries5xxBeforeFirstByte(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"upstream","message":"try again","retryable":true}}`))
			return
		}
		w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventCompleted})
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Product: "sneat", Token: tokenFunc("t")})
	_, _, _, err := ai.Collect(c.Stream(context.Background(), ai.ChatRequest{}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}
