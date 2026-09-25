package cloud

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/cloudproto"
)

func TestInteractionContextAndReportUseSameProductAndTurn(t *testing.T) {
	client := &ai.ClientContext{InstallationID: "550e8400-e29b-41d4-a716-446655440000", Feature: "chat", Client: ai.ClientInfo{Type: "cli", Name: "datatug", Version: "0.18.3"}}
	id := "550e8400-e29b-41d4-a716-446655440001"
	var chat ai.ChatRequest
	var report cloudproto.InteractionReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(cloudproto.HeaderProduct) != "datatug" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("headers: product=%q auth=%q", r.Header.Get(cloudproto.HeaderProduct), r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v0/" + cloudproto.PathChat:
			if err := json.NewDecoder(r.Body).Decode(&chat); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", cloudproto.ContentTypeSSE)
			_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventStarted, Provider: "test", Model: "m1"})
			_ = cloudproto.WriteEvent(w, ai.Event{Type: ai.EventCompleted})
		case "/v0/" + cloudproto.PathInteraction:
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL + "/v0/", Product: "datatug", ClientContext: client, Token: func(context.Context) (string, error) { return "test-token", nil }})
	ctx := WithInteractionID(context.Background(), id)
	for _, err := range c.Stream(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: ai.RoleUser, Text: "hi"}}}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ReportInteraction(ctx, cloudproto.InteractionReport{InteractionID: id, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if chat.InteractionID != id || report.InteractionID != id || chat.Product != "datatug" || report.Product != "datatug" ||
		chat.ClientContext == nil || chat.ClientContext.InstallationID != client.InstallationID ||
		report.ClientContext == nil || report.ClientContext.Client.Version != "0.18.3" {
		t.Fatalf("chat=%+v report=%+v", chat, report)
	}
}

func TestReportInteractionFailures(t *testing.T) {
	token := func(context.Context) (string, error) { return "test-token", nil }
	base := cloudproto.InteractionReport{InteractionID: "550e8400-e29b-41d4-a716-446655440001", Status: "completed"}
	badNumber := math.NaN()
	bad := base
	bad.DetectionSteps = []cloudproto.DetectionStep{{Method: "jev", Confidence: &badNumber}}
	if err := New(Config{BaseURL: "http://example.test/v0/", Product: "sneat", Token: token}).ReportInteraction(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("marshal error=%v", err)
	}
	if err := New(Config{BaseURL: "://invalid", Product: "sneat", Token: token}).ReportInteraction(context.Background(), base); err == nil {
		t.Fatal("expected invalid URL error")
	}
	if err := New(Config{BaseURL: "http://127.0.0.1:1/v0/", Product: "sneat", Token: token}).ReportInteraction(context.Background(), base); err == nil {
		t.Fatal("expected transport error")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	if err := New(Config{BaseURL: srv.URL + "/v0/", Product: "sneat", Token: token}).ReportInteraction(context.Background(), base); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("status error=%v", err)
	}
}
