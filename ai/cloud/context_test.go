package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/decision/compose"
)

func TestCloudRequestPayerHintsAreImmutablePerClient(t *testing.T) {
	chosen := New(Config{BaseURL: "https://example.test/v0/", Product: "datatug", Account: "team-1", Project: "cloud-project-1", Token: func(context.Context) (string, error) { return "token", nil }})
	plain := New(Config{BaseURL: "https://example.test/v0/", Product: "datatug", Token: func(context.Context) (string, error) { return "token", nil }})
	for _, path := range []string{cloudproto.PathChat, cloudproto.PathDecision, cloudproto.PathScore, cloudproto.PathInteraction, cloudproto.PathUsage} {
		for _, tc := range []struct {
			name    string
			client  *Client
			account string
			project string
		}{{"chosen", chosen, "team-1", "cloud-project-1"}, {"plain", plain, "", ""}} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req, err := tc.client.newRequest(context.Background(), path, nil)
				if err != nil {
					t.Fatal(err)
				}
				if req.Header.Get(cloudproto.HeaderAccount) != tc.account || req.Header.Get(cloudproto.HeaderProject) != tc.project || req.Header.Get(cloudproto.HeaderProduct) != "datatug" {
					t.Fatalf("headers = %v", req.Header)
				}
			})
		}
	}
}

func TestCloudTerminalRefusalsNeverRetryOrFailOver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		limit  string
		kind   error
	}{
		{"model class", http.StatusForbidden, ai.ErrCodeInvalid, `{"v":1,"reason":"model_class"}`, decision.ErrPolicyRefusal},
		{"question context", http.StatusConflict, ai.ErrCodeContextChanged, "", decision.ErrPolicyRefusal},
		{"quota", http.StatusTooManyRequests, ai.ErrCodeQuota, `{"v":1,"reason":"quota"}`, decision.ErrQuota},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(cloudproto.ErrorResponse{Error: ai.Error{Code: tc.code, Message: "refused", Retryable: true}, Limit: json.RawMessage(tc.limit)})
			if err != nil {
				t.Fatal(err)
			}
			refusalRoute := jsonRoute(tc.status, string(body))
			srv := newProtoServer(t, map[string]route{
				cloudproto.PathChat: refusalRoute, cloudproto.PathDecision: refusalRoute, cloudproto.PathScore: refusalRoute,
			})
			c := srv.client()
			var chatErr error
			for _, err := range c.Stream(context.Background(), ai.ChatRequest{}) {
				if err != nil {
					chatErr = err
				}
			}
			var ae *ai.Error
			if !errors.As(chatErr, &ae) {
				t.Fatalf("chat error has no *ai.Error: %v", chatErr)
			}
			if ae.IsRetryable() || string(ae.Details) != tc.limit || srv.count(cloudproto.PathChat) != 1 {
				t.Fatalf("chat err=%v details=%s hits=%d", chatErr, ae.Details, srv.count(cloudproto.PathChat))
			}
			backup := newProtoServer(t, nil)
			fallback := compose.Fallback(c.Decider(), backup.client().Decider())
			_, _, rep, err := fallback.DecideTraced(context.Background(), decideReq())
			if !errors.Is(err, tc.kind) || !errors.As(err, &ae) || ae == nil || string(ae.Details) != tc.limit || rep.FallbackFired || srv.count(cloudproto.PathDecision) != 1 || backup.count(cloudproto.PathDecision) != 0 {
				t.Fatalf("decision err=%v rep=%+v hits=%d backup=%d", err, rep, srv.count(cloudproto.PathDecision), backup.count(cloudproto.PathDecision))
			}
			_, scoreRep, err := fallback.ScoreTraced(context.Background(), libraryScoreRequest())
			if !errors.Is(err, tc.kind) || !errors.As(err, &ae) || ae == nil || string(ae.Details) != tc.limit || scoreRep.FallbackFired || srv.count(cloudproto.PathScore) != 1 || backup.count(cloudproto.PathScore) != 0 {
				t.Fatalf("score err=%v rep=%+v hits=%d backup=%d", err, scoreRep, srv.count(cloudproto.PathScore), backup.count(cloudproto.PathScore))
			}
			if tc.kind == decision.ErrPolicyRefusal {
				_, ok, tr := (decision.Chain{Providers: []decision.Provider{c.Decider(), backup.client().Decider()}}).Decide(context.Background(), decideReq())
				if ok || !errors.Is(tr.Err(), decision.ErrPolicyRefusal) || tr.StoppedBy != decision.AttemptPolicyRefusal || srv.count(cloudproto.PathDecision) != 2 || backup.count(cloudproto.PathDecision) != 0 {
					t.Fatalf("chain ok=%v trace=%+v hits=%d backup=%d", ok, tr, srv.count(cloudproto.PathDecision), backup.count(cloudproto.PathDecision))
				}
			}
		})
	}
}

func TestCloudRefusalPreservesOuterDetailsAndTerminalClassification(t *testing.T) {
	limit := json.RawMessage(`{"v":1,"reason":"model_class","left":2}`)
	data, err := json.Marshal(cloudproto.ErrorResponse{Error: ai.Error{Code: ai.ErrCodeInvalid, Message: "model unavailable"}, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeErrorBody(&http.Response{StatusCode: http.StatusForbidden}, data)
	if got.Code != ai.ErrCodeInvalid || string(got.Details) != string(limit) || !errors.Is(engineKind(http.StatusForbidden, got), decision.ErrPolicyRefusal) {
		t.Fatalf("refusal = %+v, kind=%v", got, engineKind(http.StatusForbidden, got))
	}
	if !errors.Is(engineKind(http.StatusConflict, &ai.Error{Code: ai.ErrCodeContextChanged}), decision.ErrPolicyRefusal) {
		t.Fatal("question context change must stop decision fallback")
	}
	if !errors.Is(engineKind(http.StatusForbidden, &ai.Error{Code: ai.ErrCodeAuth}), decision.ErrAuth) {
		t.Fatal("plain 403 remains authentication failure")
	}
}
