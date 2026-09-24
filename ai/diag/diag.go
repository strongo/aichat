// Package diag is the shared diagnostics record for one conversational turn:
// which path handled it, what the decision chain did, which LLM answered,
// timing and usage. It never carries user text or API keys.
package diag

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

// Path names how a turn was handled.
type Path string

const (
	PathDeterministic Path = "deterministic" // a decision.Provider decided AND the product handled it without an LLM
	PathDecision      Path = "decision"      // a decision.Provider decided and the product's own logic + a scoped LLM call handled it
	PathLLM           Path = "llm"           // no decider was configured/enabled: single main-LLM inference
	PathLLMFallback   Path = "llm-fallback"  // every decider abstained/errored/timed out: fell through to the main LLM
)

// Turn is one conversational turn's diagnostic record. Log it at Debug so it
// never appears by default but is available when investigating routing or
// cost. Never populate it with user message text or secrets.
type Turn struct {
	Path Path `json:"path"`
	// Decision is the chain's trace when a decision chain ran (nil on
	// PathLLM, where there was no decider to run).
	Decision *decision.Trace `json:"decision,omitempty"`
	Module   string          `json:"module,omitempty"`
	Intent   string          `json:"intent,omitempty"`
	// RequiredScopes is what the decision asked for; ActualScopes is what
	// ctxmgr.Select actually sent (after retain/compact), so a gap between
	// them is visible in one record.
	RequiredScopes []string `json:"requiredScopes,omitempty"`
	ActualScopes   []string `json:"actualScopes,omitempty"`
	// LLMSkipped is true when the decision alone answered the turn (no
	// further inference was made).
	LLMSkipped bool      `json:"llmSkipped"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	Usage      *ai.Usage `json:"usage,omitempty"`

	DecisionLatency time.Duration `json:"decisionLatency,omitempty"`
	LLMLatency      time.Duration `json:"llmLatency,omitempty"`

	// Errors are short CLASSIFIERS (an *ai.Error.Code such as "upstream" or
	// "rate_limited", or "error" for anything else) collected while handling
	// the turn -- NEVER a raw error message or upstream response body, which
	// can carry arbitrary user content or provider-internal detail this
	// package promises never to log. Build entries with ErrorCode, don't
	// append err.Error() directly.
	Errors []string `json:"errors,omitempty"`
}

// ErrorCode classifies err down to a short, safe-to-log string: an
// *ai.Error's Code when err is (or wraps) one, or "error" otherwise. It
// never returns err's message text. Use it to build Turn.Errors:
//
//	t.Errors = append(t.Errors, diag.ErrorCode(err))
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var aiErr *ai.Error
	if errors.As(err, &aiErr) && aiErr.Code != "" {
		return aiErr.Code
	}
	return "error"
}

// Log emits t at Debug level via logger. It is a thin wrapper so callers
// have one place to point at ("log this turn") and so the level (and the
// "never log user text" rule) live in one spot.
func Log(ctx context.Context, logger *slog.Logger, t Turn) {
	if logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("path", string(t.Path)),
		slog.String("module", t.Module),
		slog.String("intent", t.Intent),
		slog.Bool("llmSkipped", t.LLMSkipped),
		slog.String("provider", t.Provider),
		slog.String("model", t.Model),
		slog.Duration("decisionLatency", t.DecisionLatency),
		slog.Duration("llmLatency", t.LLMLatency),
	}
	if t.Decision != nil {
		attrs = append(attrs, slog.String("decidedBy", t.Decision.DecidedBy), slog.Int("decisionAttempts", len(t.Decision.Attempts)))
	}
	if len(t.RequiredScopes) > 0 {
		attrs = append(attrs, slog.Any("requiredScopes", t.RequiredScopes))
	}
	if len(t.ActualScopes) > 0 {
		attrs = append(attrs, slog.Any("actualScopes", t.ActualScopes))
	}
	if t.Usage != nil {
		attrs = append(attrs,
			slog.Int64("inputTokens", t.Usage.InputTokens),
			slog.Int64("outputTokens", t.Usage.OutputTokens),
			slog.Int64("cacheReadTokens", t.Usage.CacheReadTokens),
			slog.Int64("cacheWriteTokens", t.Usage.CacheWriteTokens),
		)
		if t.Usage.Allowance != nil {
			attrs = append(attrs,
				slog.String("allowanceUnit", t.Usage.Allowance.Unit),
				slog.Int64("allowanceUsed", t.Usage.Allowance.Used),
				slog.Int64("allowanceLimit", t.Usage.Allowance.Limit),
			)
		}
	}
	if len(t.Errors) > 0 {
		attrs = append(attrs, slog.Any("errors", t.Errors))
	}
	logger.LogAttrs(ctx, slog.LevelDebug, "aichat.turn", attrs...)
}
