package typesafe

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinels matched with errors.Is against an *APIError (or the errors this
// package returns for a malformed exchange).
var (
	// ErrAuth: the API key is missing, invalid or not allowed (HTTP 401/403).
	ErrAuth = errors.New("typesafe: authentication failed")
	// ErrInvalidRequest: the request body failed validation (HTTP 400/422;
	// the documented status is 422, the live API answers 400).
	ErrInvalidRequest = errors.New("typesafe: invalid request")
	// ErrRateLimited: the rate limit was exceeded (HTTP 429). The caller (a
	// combinator or breaker) decides whether to try another engine; this package
	// never retries.
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
	// ErrTooManyOptions: a Choice has more than MaxChoiceOptions options.
	ErrTooManyOptions = errors.New("typesafe: too many choice options")
)

// APIError is a non-2xx answer from the API. It carries only the status, the
// error type and the request id; it never carries the response body, which can
// echo the submitted state.
type APIError struct {
	Status int
	// ErrorType is the API's machine-readable type (for example
	// "authentication_error"), when the body had one.
	ErrorType string
	// RequestID is the x-typesafe-request-id response header, for support.
	RequestID string
	// RetryAfter is the Retry-After header, when present.
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

// Is makes errors.Is(err, ErrRateLimited) and friends work.
func (e *APIError) Is(target error) bool { return target == e.kind }

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

// parseRetryAfter reads a Retry-After header given in seconds.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
