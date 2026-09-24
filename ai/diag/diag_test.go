package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

func TestLog_EmitsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	turn := Turn{
		Path:            PathDecision,
		Decision:        &decision.Trace{DecidedBy: "rules", Attempts: []decision.Attempt{{Provider: "rules", Outcome: "decided"}}},
		Module:          "calendar",
		Intent:          "show",
		RequiredScopes:  []string{"calendar"},
		ActualScopes:    []string{"calendar", "tasks"},
		LLMSkipped:      true,
		Provider:        "anthropic",
		Model:           "claude-haiku-4-5-20251001",
		Usage:           &ai.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3},
		DecisionLatency: 12 * time.Millisecond,
		LLMLatency:      340 * time.Millisecond,
		Errors:          []string{"rate_limited: slow down"},
	}
	Log(context.Background(), logger, turn)

	out := buf.String()
	if !strings.Contains(out, `"msg":"aichat.turn"`) {
		t.Fatalf("output missing message: %s", out)
	}
	if !strings.Contains(out, `"level":"DEBUG"`) {
		t.Fatalf("expected Debug level: %s", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if parsed["module"] != "calendar" || parsed["intent"] != "show" {
		t.Errorf("parsed = %+v", parsed)
	}
	if parsed["decidedBy"] != "rules" {
		t.Errorf("decidedBy = %v", parsed["decidedBy"])
	}
	if parsed["path"] != string(PathDecision) {
		t.Errorf("path = %v", parsed["path"])
	}
}

func TestLog_NeverIncludesUserText(t *testing.T) {
	// Turn has no field for raw user text at all; this test documents and
	// pins that contract so a future field addition doesn't quietly leak it.
	turn := Turn{Path: PathLLM}
	b, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "text") || strings.Contains(lk, "message") || strings.Contains(lk, "key") || strings.Contains(lk, "token") && !strings.Contains(lk, "tokens") {
			t.Errorf("Turn JSON has a suspicious field %q -- diag.Turn must never carry user text or secrets", k)
		}
	}
}

func TestLog_NilLoggerIsNoOp(t *testing.T) {
	// Must not panic.
	Log(context.Background(), nil, Turn{Path: PathLLM})
}

func TestPaths_Distinct(t *testing.T) {
	paths := []Path{PathDeterministic, PathDecision, PathLLM, PathLLMFallback}
	seen := map[Path]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Errorf("duplicate path value %q", p)
		}
		seen[p] = true
	}
}
