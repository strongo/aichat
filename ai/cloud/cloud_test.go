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
