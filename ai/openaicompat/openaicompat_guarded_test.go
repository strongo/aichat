package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
)

type guardedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f guardedRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func guardedResponse(r *http.Request, status int, body string, h http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

const guardedSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func guardedProvider(rt http.RoundTripper, field OutputTokenField, policy *GuardedPolicy) *Provider {
	return New(Config{BaseURL: "https://example.invalid/v1", APIKey: "test-key", Model: "model", OutputTokenField: field,
		Guarded: policy, HTTPClient: &http.Client{Transport: rt}})
}

func guardedRequest() ai.ChatRequest {
	return ai.ChatRequest{MaxTokens: 17, Messages: []ai.Message{{Role: ai.RoleUser, Text: "hello"}}}
}

func assertFatalBeforeStart(t *testing.T, p *Provider, ctx context.Context, req ai.ChatRequest, code string) {
	t.Helper()
	count := 0
	for ev, err := range p.Stream(ctx, req) {
		count++
		var ae *ai.Error
		if !errors.As(err, &ae) || ev.Type != ai.EventError || ev.Error != ae || ae.Code != code {
			t.Fatalf("event %d: event=%+v err=%v, want one fatal %s", count, ev, err, code)
		}
	}
	if count != 1 {
		t.Fatalf("events=%d, want exactly one fatal event", count)
	}
}

func TestGuardedWireAndLegacyDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field OutputTokenField
		guard *GuardedPolicy
		want  string
	}{
		{"guarded-default", "", &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}, "max_completion_tokens"},
		{"guarded-explicit-legacy", OutputTokenFieldMaxCompletionTokens, &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}, "max_completion_tokens"},
		{"guarded-max-tokens", OutputTokenFieldMaxTokens, &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}, "max_tokens"},
		{"legacy-default", "", nil, "max_completion_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent atomic.Int32
			p := guardedProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent.Add(1)
				if r.Method != http.MethodPost || r.URL.String() != "https://example.invalid/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("request: %s %s %v", r.Method, r.URL, r.Header)
				}
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(b, &got); err != nil {
					t.Fatal(err)
				}
				if string(got[tc.want]) != "17" {
					t.Errorf("%s=%s, body=%s", tc.want, got[tc.want], b)
				}
				other := "max_tokens"
				if tc.want == other {
					other = "max_completion_tokens"
				}
				if _, ok := got[other]; ok {
					t.Errorf("both output fields in %s", b)
				}
				var messages []chatMessage
				if err := json.Unmarshal(got["messages"], &messages); err != nil {
					t.Fatal(err)
				}
				if len(messages) != 3 || messages[0].Role != "system" || !strings.Contains(messages[0].Content, "static") ||
					messages[1].Role != "assistant" || messages[1].Content != "history" ||
					messages[2].Role != "user" || !strings.Contains(messages[2].Content, "dynamic") || !strings.HasSuffix(messages[2].Content, "hello") {
					t.Errorf("transformed messages = %+v", messages)
				}
				var format responseFormat
				if err := json.Unmarshal(got["response_format"], &format); err != nil {
					t.Fatal(err)
				}
				var tools []toolDef
				if err := json.Unmarshal(got["tools"], &tools); err != nil {
					t.Fatal(err)
				}
				if string(got["model"]) != `"model"` || string(got["stream"]) != "true" ||
					format.Type != "json_schema" || !format.JSONSchema.Strict || len(tools) != 1 || tools[0].Function.Name != "lookup" {
					t.Errorf("incomplete transformed request: %s", b)
				}
				return guardedResponse(r, http.StatusOK, guardedSSE, http.Header{"Content-Type": {"text/event-stream"}}), nil
			}), tc.field, tc.guard)
			req := guardedRequest()
			req.System = "system"
			req.Context = []ai.ContextBlock{{Kind: ai.ContextStatic, Name: "skill", Text: "static"}, {Kind: ai.ContextDynamic, Name: "now", Text: "dynamic"}}
			req.Messages = append([]ai.Message{{Role: ai.RoleAssistant, Text: "history"}}, req.Messages...)
			req.ResponseSchema = json.RawMessage(`{"type":"object"}`)
			req.Tools = []ai.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
			text, _, _, err := ai.Collect(p.Stream(context.Background(), req))
			if err != nil || text != "ok" || sent.Load() != 1 {
				t.Fatalf("text=%q err=%v sends=%d", text, err, sent.Load())
			}
		})
	}
	var body []byte
	p := guardedProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		return guardedResponse(r, http.StatusOK, guardedSSE, nil), nil
	}), "", nil)
	if _, _, _, err := ai.Collect(p.Stream(context.Background(), ai.ChatRequest{})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "max_completion_tokens") || strings.Contains(string(body), `"max_tokens"`) {
		t.Fatalf("legacy zero limit changed: %s", body)
	}
}

func TestGuardedPreflightRejectsWithoutSend(t *testing.T) {
	var sent atomic.Int32
	rt := guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return guardedResponse(r, http.StatusOK, guardedSSE, nil), nil
	})
	valid := GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}
	for _, tc := range []struct {
		name   string
		field  OutputTokenField
		policy GuardedPolicy
		tokens int
	}{
		{"zero-attempts", "", GuardedPolicy{17, 0}, 17},
		{"negative-attempts", "", GuardedPolicy{17, -1}, 17},
		{"multiple-attempts", "", GuardedPolicy{17, 2}, 17},
		{"zero-policy-cap", "", GuardedPolicy{0, 1}, 17},
		{"negative-policy-cap", "", GuardedPolicy{-1, 1}, 17},
		{"zero-request-cap", "", valid, 0},
		{"negative-request-cap", "", valid, -1},
		{"above-policy-cap", "", valid, 18},
		{"unknown-field", "unknown", valid, 17},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := guardedProvider(rt, tc.field, &tc.policy)
			req := guardedRequest()
			req.MaxTokens = tc.tokens
			assertFatalBeforeStart(t, p, context.Background(), req, ai.ErrCodeInvalid)
		})
	}
	assertFatalBeforeStart(t, guardedProvider(rt, "unknown", nil), context.Background(), guardedRequest(), ai.ErrCodeInvalid)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertFatalBeforeStart(t, guardedProvider(rt, "", &valid), ctx, guardedRequest(), ai.ErrCodeCanceled)
	if sent.Load() != 0 {
		t.Fatalf("preflight sent %d HTTP requests", sent.Load())
	}
}

func TestGuardedOneAttemptErrorsAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		header http.Header
		failed error
		code   string
	}{
		{"rate-limit", 429, `{"error":{"message":"slow"}}`, http.Header{"Retry-After": {"60"}}, nil, ai.ErrCodeRateLimited},
		{"server-error", 503, `{"error":{"message":"down"}}`, nil, nil, ai.ErrCodeUpstream},
		{"transport-timeout", 0, "", nil, context.DeadlineExceeded, ai.ErrCodeUpstream},
		{"redirect-307", 307, "", http.Header{"Location": {"https://elsewhere.invalid/collect"}}, nil, ai.ErrCodeInvalid},
		{"redirect-308", 308, "", http.Header{"Location": {"https://elsewhere.invalid/collect"}}, nil, ai.ErrCodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent, callbacks atomic.Int32
			client := &http.Client{Transport: guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent.Add(1)
				if tc.failed != nil {
					return nil, tc.failed
				}
				return guardedResponse(r, tc.status, tc.body, tc.header), nil
			}), CheckRedirect: func(*http.Request, []*http.Request) error { callbacks.Add(1); return nil }}
			p := New(Config{BaseURL: "https://example.invalid/v1", Model: "model", HTTPClient: client,
				Guarded: &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}})
			start := time.Now()
			assertFatalBeforeStart(t, p, context.Background(), guardedRequest(), tc.code)
			if sent.Load() != 1 || callbacks.Load() != 0 || time.Since(start) > 3*time.Second {
				t.Fatalf("sends=%d callbacks=%d elapsed=%s", sent.Load(), callbacks.Load(), time.Since(start))
			}
			if client.CheckRedirect == nil {
				t.Fatal("mutated caller's redirect policy")
			}
		})
	}
}

func TestGuardedReasoning400ConsumesBudgetAndRemembersModel(t *testing.T) {
	var sent atomic.Int32
	p := guardedProvider(guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "reasoning_effort") {
			return guardedResponse(r, 400, `{"error":{"message":"Unsupported parameter: 'reasoning_effort'"}}`, nil), nil
		}
		return guardedResponse(r, http.StatusOK, guardedSSE, nil), nil
	}), "", &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1})
	req := guardedRequest()
	req.Reasoning = ai.ReasoningHigh
	assertFatalBeforeStart(t, p, context.Background(), req, ai.ErrCodeInvalid)
	if sent.Load() != 1 {
		t.Fatalf("compatibility resend occurred: %d", sent.Load())
	}
	text, _, _, err := ai.Collect(p.Stream(context.Background(), req))
	if err != nil || text != "ok" || sent.Load() != 2 {
		t.Fatalf("independent later call: text=%q err=%v sends=%d", text, err, sent.Load())
	}
}

func TestGuardedSnapshotsAndConcurrentBudgets(t *testing.T) {
	var oldSends, newSends atomic.Int32
	oldTransport := guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		oldSends.Add(1)
		if r.Header.Get("X-Guard") != "original" {
			t.Errorf("mutated header leaked: %q", r.Header.Get("X-Guard"))
		}
		return guardedResponse(r, http.StatusOK, guardedSSE, nil), nil
	})
	newTransport := guardedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		newSends.Add(1)
		return guardedResponse(r, http.StatusOK, guardedSSE, nil), nil
	})
	policy := &GuardedPolicy{MaxOutputTokens: 17, MaxOutboundAttempts: 1}
	headers := map[string]string{"X-Guard": "original"}
	client := &http.Client{Transport: oldTransport}
	p := New(Config{BaseURL: "https://example.invalid/v1", Model: "model", Headers: headers,
		HTTPClient: client, Guarded: policy})
	policy.MaxOutputTokens = 0
	policy.MaxOutboundAttempts = 3
	headers["X-Guard"] = "mutated"
	client.Transport = newTransport
	const count = 8
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, _, _, err := ai.Collect(p.Stream(context.Background(), guardedRequest()))
			if err != nil || text != "ok" {
				t.Errorf("text=%q err=%v", text, err)
			}
		}()
	}
	wg.Wait()
	if oldSends.Load() != count || newSends.Load() != 0 {
		t.Fatalf("snapshot sends: old=%d new=%d", oldSends.Load(), newSends.Load())
	}
}
