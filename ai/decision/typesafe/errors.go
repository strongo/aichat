package typesafe

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// sentinel is an error value with a fixed message that also matches (errors.Is)
// a parent error: ErrAuth matches decision.ErrAuth, and the local request checks
// match ErrInvalidRequest and so decision.ErrInvalidRequest, which is how a
// circuit breaker learns that the caller, not the engine, is at fault.
type sentinel struct {
	msg    string
	parent error
}

func (s *sentinel) Error() string { return s.msg }
func (s *sentinel) Unwrap() error { return s.parent }

// Sentinels matched with errors.Is against an *APIError (or the errors this
// package returns for a malformed exchange).
var (
	// ErrAuth: the API key is missing, invalid or not allowed (HTTP 401/403).
	// It matches decision.ErrAuth too. It is not an engine-health failure: a
	// circuit breaker does not count it, and combinators record it as "auth".
	ErrAuth error = &sentinel{"typesafe: authentication failed", decision.ErrAuth}
	// ErrInvalidRequest: the request body failed validation (HTTP 400/422;
	// the documented status is 422, the live API answers 400), or a local check
	// refused it before any call. It matches decision.ErrInvalidRequest too: the
	// caller's request is at fault, not the engine, so a circuit breaker does not
	// count it.
	ErrInvalidRequest error = &sentinel{"typesafe: invalid request", decision.ErrInvalidRequest}
	// ErrRateLimited: the rate limit was exceeded (HTTP 429). The caller (a
	// combinator or breaker) decides whether to try another engine; this package
	// never retries.
	//
	// It is deliberately NOT decision.ErrQuota. TypeSafe's documentation (the
	// error classes of its SDKs and the llms.txt index, checked 2026-10) defines
	// one 429 error, "the rate limit was exceeded", with a Retry-After delay, and
	// no error type, status or body field that tells a billing or credit
	// exhaustion from a per-second rate limit; the API documents no quota error at
	// all. So every 429 is treated as a transient rate limit: a breaker counts it
	// and opens for the Retry-After, a combinator may fail over. A product that
	// needs a hard allowance stop (the anonymous demo) must enforce it on its own
	// side, for example behind the cloud boundary, whose protocol does carry code
	// "quota" (ai/cloud maps it to decision.ErrQuota), and must cap a paid backup
	// that follows this engine with compose.NewBudget (decision.ErrBudget), which is
	// what bounds the bill when an exhausted account looks like a rate limit. A 402
	// is not documented either (llms.txt and the SDK error classes, checked
	// 2026-10), so it is not mapped to decision.ErrQuota: it is an unexpected
	// status, a plain engine failure. If TypeSafe documents a distinguishing error
	// type or status, kindForStatus is the one place to map it.
	ErrRateLimited = errors.New("typesafe: rate limited")
	// ErrOverloaded: the service is temporarily overloaded (HTTP 529).
	ErrOverloaded = errors.New("typesafe: overloaded")
	// ErrServer: any other 5xx.
	ErrServer = errors.New("typesafe: server error")
	// ErrUnexpectedStatus: any other non-2xx status.
	ErrUnexpectedStatus = errors.New("typesafe: unexpected HTTP status")
	// ErrBadResponse: a 2xx response that is not the documented shape, or whose
	// answers do not match the questions asked.
	ErrBadResponse = errors.New("typesafe: malformed response")
	// ErrTooManyOptions: a Choice has more than MaxChoiceOptions options. Checked
	// locally; also an ErrInvalidRequest.
	ErrTooManyOptions error = &sentinel{"typesafe: too many choice options", ErrInvalidRequest}
	// ErrTooManyQuestions: a call carries more than MaxQuestionsPerCall
	// questions (a relevance question costs one per candidate). Checked locally;
	// also an ErrInvalidRequest.
	ErrTooManyQuestions error = &sentinel{"typesafe: too many questions in one call", ErrInvalidRequest}
	// ErrStateTooLarge: the state plus the longest question is estimated above
	// Config.MaxStateTokens. The estimate is approximate (bytes/4). Checked
	// locally; also an ErrInvalidRequest.
	ErrStateTooLarge error = &sentinel{"typesafe: state too large", ErrInvalidRequest}
)

// APIError is a non-2xx answer from the API. It carries only the status, the
// error type, the request id and the retry delay; it never carries the response
// body, which can echo the submitted state.
type APIError struct {
	Status int
	// ErrorType is the API's machine-readable type (for example
	// "authentication_error"), when the body had one.
	ErrorType string
	// RequestID is the x-typesafe-request-id response header, for support.
	RequestID string
	// RetryAfter is the Retry-After header (seconds or an HTTP date), when
	// present and in the future.
	RetryAfter time.Duration
	kind       error
}

// Error implements error.
func (e *APIError) Error() string {
	s := fmt.Sprintf("typesafe: HTTP %d (%s)", e.Status, strings.TrimPrefix(e.kind.Error(), "typesafe: "))
	if e.ErrorType != "" {
		s += " " + e.ErrorType
	}
	if e.RequestID != "" {
		s += " request " + e.RequestID
	}
	return s
}

// Unwrap makes errors.Is(err, ErrRateLimited), errors.Is(err, ErrAuth) and
// friends work, and lets errors.Is(err, decision.ErrAuth) and
// decision.ErrInvalidRequest see through to the engine-neutral sentinels.
func (e *APIError) Unwrap() error { return e.kind }

// RetryDelay implements decision.RetryDelayer: a circuit breaker keeps the
// engine out of service at least this long.
func (e *APIError) RetryDelay() time.Duration { return e.RetryAfter }

// kindForStatus maps an HTTP status to its sentinel.
func kindForStatus(status int) error {
	switch {
	case status == 401 || status == 403:
		return ErrAuth
	case status == 400 || status == 422:
		return ErrInvalidRequest
	case status == 429:
		return ErrRateLimited
	case status == 529:
		return ErrOverloaded
	case status >= 500:
		return ErrServer
	default:
		return ErrUnexpectedStatus
	}
}
