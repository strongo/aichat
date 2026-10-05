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

func TestCloudRefusalPreservesOuterDetailsAndTerminalClassification(t *testing.T) {
	limit := json.RawMessage(`{"v":1,"reason":"model_class","left":2}`)
	data, err := json.Marshal(cloudproto.ErrorResponse{Error: ai.Error{Code: ai.ErrCodeInvalid, Message: "model unavailable"}, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeErrorBody(&http.Response{StatusCode: http.StatusForbidden}, data)
	if got.Code != ai.ErrCodeInvalid || string(got.Details) != string(limit) || !errors.Is(engineKind(http.StatusForbidden, got), decision.ErrInvalidRequest) {
		t.Fatalf("refusal = %+v, kind=%v", got, engineKind(http.StatusForbidden, got))
	}
	if !errors.Is(engineKind(http.StatusConflict, &ai.Error{Code: ai.ErrCodeContextChanged}), decision.ErrInvalidRequest) {
		t.Fatal("question context change must stop decision fallback")
	}
	if !errors.Is(engineKind(http.StatusForbidden, &ai.Error{Code: ai.ErrCodeAuth}), decision.ErrAuth) {
		t.Fatal("plain 403 remains authentication failure")
	}
}
