package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		Path:           PathDecision,
		Decision:       &decision.Trace{DecidedBy: "rules", Attempts: []decision.Attempt{{Provider: "rules", Outcome: "decided"}}},
		Module:         "calendar",
		Intent:         "show",
		RequiredScopes: []string{"calendar"},
		ActualScopes:   []string{"calendar", "tasks"},
		LLMSkipped:     true,
		Provider:       "anthropic",
		Model:          "claude-haiku-4-5-20251001",
		Usage: &ai.Usage{
			InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2,
			Allowance: &ai.Allowance{Unit: "tokens", Used: 100, Limit: 1000},
		},
		DecisionLatency: 12 * time.Millisecond,
		LLMLatency:      340 * time.Millisecond,
		Errors:          []string{ErrorCode(&ai.Error{Code: ai.ErrCodeRateLimited, Message: "slow down"})},
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
	if parsed["cacheWriteTokens"] != float64(2) {
		t.Errorf("cacheWriteTokens = %v, want 2", parsed["cacheWriteTokens"])
	}
	if parsed["allowanceUnit"] != "tokens" || parsed["allowanceUsed"] != float64(100) || parsed["allowanceLimit"] != float64(1000) {
		t.Errorf("allowance fields missing/wrong: %+v", parsed)
	}
	if errs, ok := parsed["errors"].([]any); !ok || len(errs) != 1 || errs[0] != "rate_limited" {
		t.Errorf("errors = %v, want [\"rate_limited\"] (a code, not the raw message)", parsed["errors"])
	}
}

func TestLog_PopulatedTurnNeverLeaksASentinelUserString(t *testing.T) {
	// A zero-value Turn trivially has nothing to leak. This instead builds a
	// FULLY populated Turn -- every field set to something realistic -- and
	// checks that a sentinel standing in for private user message text
	// never appears anywhere in the logged output, proving there is no path
	// (not even an indirect one, like an error message) that carries it.
	const sentinel = "USER-SECRET-ABC123-do-not-log-me"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	turn := Turn{
		Path: PathLLMFallback,
		Decision: &decision.Trace{
			DecidedBy: "llm-decider",
			Attempts: []decision.Attempt{
				{Provider: "rules", Outcome: "abstained"},
				{Provider: "llm-decider", Outcome: "decided", Detail: "module=0.90 intent=0.85"},
			},
		},
		Module:         "calendar",
		Intent:         "reschedule",
		RequiredScopes: []string{"calendar", "calendar.happenings"},
		ActualScopes:   []string{"calendar", "calendar.happenings", "tasks"},
		LLMSkipped:     false,
		Provider:       "openai-compatible",
		Model:          "gpt-5",
		Usage: &ai.Usage{
			InputTokens: 500, OutputTokens: 120, CacheReadTokens: 80, CacheWriteTokens: 20,
			Allowance: &ai.Allowance{Unit: "requests", Used: 3, Limit: 100},
		},
		DecisionLatency: 8 * time.Millisecond,
		LLMLatency:      612 * time.Millisecond,
		Errors:          []string{ErrorCode(&ai.Error{Code: ai.ErrCodeUpstream, Message: sentinel})},
	}
	Log(context.Background(), logger, turn)

	b, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), sentinel) {
		t.Fatalf("Turn JSON leaked the sentinel: %s", b)
	}
	if strings.Contains(buf.String(), sentinel) {
		t.Fatalf("Log() output leaked the sentinel: %s", buf.String())
	}

	// Same field-name heuristic as before, now against a fully populated
	// struct (reflection-based marshaling can behave differently for a zero
	// vs. non-zero field, e.g. via omitempty).
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "text") || strings.Contains(lk, "message") || strings.Contains(lk, "key") || (strings.Contains(lk, "token") && !strings.Contains(lk, "tokens")) {
			t.Errorf("Turn JSON has a suspicious field %q -- diag.Turn must never carry user text or secrets", k)
		}
	}
}

func TestLog_NilLoggerIsNoOp(t *testing.T) {
	// Must not panic.
	Log(context.Background(), nil, Turn{Path: PathLLM})
}

func TestErrorCode(t *testing.T) {
	if got := ErrorCode(nil); got != "" {
		t.Errorf("ErrorCode(nil) = %q, want empty", got)
	}
	aiErr := &ai.Error{Code: ai.ErrCodeRateLimited, Message: "some upstream body that must not leak"}
	if got := ErrorCode(aiErr); got != ai.ErrCodeRateLimited {
		t.Errorf("ErrorCode(aiErr) = %q, want %q", got, ai.ErrCodeRateLimited)
	}
	wrapped := fmt.Errorf("context: %w", aiErr)
	if got := ErrorCode(wrapped); got != ai.ErrCodeRateLimited {
		t.Errorf("ErrorCode(wrapped) = %q, want the wrapped *ai.Error's code", got)
	}
	plain := errors.New("boom")
	if got := ErrorCode(plain); got != "error" {
		t.Errorf("ErrorCode(plain) = %q, want \"error\"", got)
	}
}
