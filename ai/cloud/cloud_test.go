package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
	var p decision.Provider = c
	if p.Name() != "cloud" {
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
	_, ok, err := c.Decide(context.Background(), decision.Request{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if ok {
		t.Fatal("expected abstention when decided=false")
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
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}
