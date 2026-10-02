// Package typesafe is a client for TypeSafe AI's System One API (the "Jev"
// decision model) and a decision.Provider / decision.ScoredProvider built on
// it.
//
// You supply state and typed questions; the API returns a probability for each
// option. It does not write text. Three question types exist: Choice (one of N
// options, with a probability per option and a confidence), Score (an ordered
// rubric) and Noul (a yes/no probability, no confidence). Several questions go
// in one call and are answered independently, in parallel, against the same
// state.
//
// Properties of this package that callers can rely on:
//
//   - HTTP goes through the HTTPDoer interface; tests use a fake and never the
//     network. The default client never follows redirects (a 307/308 would
//     re-send the state, and the bearer key, to another host), and the base URL
//     must be https (http only for a loopback host, for tests). A caller that
//     supplies its own HTTPDoer must make it refuse redirects too.
//   - There are no retries here. Failing over to another engine, and not hammering
//     an engine that is down, belong to the compose combinators and breaker.
//   - Nothing is logged. Error values carry a status, an error type, a request
//     id and counts, never the response body, the submitted state or any
//     candidate or question id. The API key is only ever placed in the
//     Authorization header and is redacted from String().
//   - Answers are calibrated probabilities, so the decision types it returns have
//     Calibrated set. Calibration holds for one model version: Config.Model is
//     required, pin a versioned id so thresholds measured against it stay valid,
//     and the model id the API reports is recorded in every Decision and
//     ScoreResult.
//   - Requests are checked locally before any call (state size, number of
//     questions, number of options); a refused request is an error that matches
//     decision.ErrInvalidRequest, which a circuit breaker does not count against
//     the engine.
//
// # What is sent
//
// The state is built from the request's text, recent turns, session entity
// titles, Context and taxonomy or candidate descriptions, and all of it is sent
// verbatim to the engine's operator. Callers must send metadata (names, schemas,
// public descriptions), never row data, credentials or user identifiers.
//
// The package is product-neutral: the product supplies the taxonomy.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// Defaults and API limits (from the published model documentation).
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	// ModelLatest is the alias of the most recent stable release. It moves when
	// TypeSafe releases a model, and probabilities, and so every threshold
	// measured against them, move with it: use it only as an explicit choice
	// (for example while measuring). Production configuration should pin a
	// versioned id such as "jev-1.13.0".
	ModelLatest = "jev-latest"
	// DefaultName names the engine in traces.
	DefaultName = "typesafe"
	// DefaultTimeout is how long decision.Chain should wait for one call. The
	// service answers in a fraction of a second; this bounds a bad day.
	DefaultTimeout = 2 * time.Second
	// DefaultMaxResponseBytes bounds how much of a response is read.
	DefaultMaxResponseBytes = 1 << 20

	// MaxChoiceOptions is the API's limit on options in one Choice.
	MaxChoiceOptions = 255
	// MaxScoreLevels is the API's limit on levels in one Score.
	MaxScoreLevels = 10
	// DefaultMaxStateTokens is the published limit on the state plus the longest
	// question (32k tokens). It is checked locally with an approximate estimate
	// (bytes/4), so it can differ from the API's own tokenizer.
	DefaultMaxStateTokens = 32000
	// MaxQuestionsPerCall is a local guard against a runaway request (a relevance
	// question costs one wire question per candidate); it is not a published
	// limit.
	MaxQuestionsPerCall = 255

	systemOnePath = "/v1/systemone"
)

// HTTPDoer is the part of *http.Client the package uses.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// CallEvent describes one finished API call for metering and telemetry. It
// holds no state, question or answer text.
type CallEvent struct {
	Engine    string
	Model     string // the versioned model id that answered ("" on failure)
	Usage     Usage
	Questions int
	Latency   time.Duration
	// Status is the HTTP status, 0 when no response arrived.
	Status int
	// Err is nil on success.
	Err error
}

// Usage is the token usage the API reports.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Config configures a Client.
type Config struct {
	// APIKey is required. It is never logged or returned in an error.
	APIKey string
	// BaseURL defaults to DefaultBaseURL. It must be https, except that http is
	// accepted for a loopback host (tests), and it must not carry credentials.
	BaseURL string
	// Model is the model id to ask, and is REQUIRED: pin a versioned id such as
	// "jev-1.13.0" so the probabilities, and the thresholds measured on them,
	// stay valid. ModelLatest is available as an explicit choice.
	Model string
	// Name is the engine's name in decision.Trace (default DefaultName).
	Name string
	// HTTPClient defaults to an *http.Client that never follows redirects and has
	// no client-side timeout (the context bounds each call). A client you supply
	// must refuse redirects too: following one re-sends the state and the key to
	// whatever host answers.
	HTTPClient HTTPDoer
	// Timeout is reported by DecisionTimeout (default DefaultTimeout).
	Timeout time.Duration
	// MaxResponseBytes bounds how much of a response is read.
	MaxResponseBytes int64
	// MaxStateTokens bounds the estimated size of the state plus the longest
	// question (default DefaultMaxStateTokens; negative disables the check). The
	// estimate is approximate (bytes/4).
	MaxStateTokens int
	// OnCall, when set, receives a CallEvent after every call.
	OnCall func(CallEvent)
	// Clock is the time source for latency (default time.Now); tests only.
	Clock func() time.Time
}

// Client calls the System One endpoint. It is safe for concurrent use.
type Client struct {
	cfg    Config
	policy *decision.SelectionPolicy
}

// New builds a Client. It fails when APIKey or Model is empty or BaseURL is not
// acceptable (see Config).
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("typesafe: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("typesafe: Model is required (pin a versioned model id; ModelLatest is an explicit choice)")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if err := checkBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			// Never follow a redirect: it would re-send the state and the key.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if cfg.MaxStateTokens == 0 {
		cfg.MaxStateTokens = DefaultMaxStateTokens
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Client{cfg: cfg}, nil
}

// checkBaseURL accepts https, and http only for a loopback host. The error
// never repeats the URL (it might carry credentials).
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return errors.New("typesafe: BaseURL is not a valid URL")
	}
	if u.User != nil {
		return errors.New("typesafe: BaseURL must not carry credentials")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return nil
	default:
		return errors.New("typesafe: BaseURL must be https (http is accepted only for a loopback host)")
	}
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// String describes the client without its key.
func (c *Client) String() string {
	return fmt.Sprintf("typesafe.Client{name:%s baseURL:%s model:%s}", c.cfg.Name, c.cfg.BaseURL, c.cfg.Model)
}

// Name implements decision.Provider and decision.ScoredProvider.
func (c *Client) Name() string { return c.cfg.Name }

// DecisionTimeout implements the optional interface decision.Chain honours.
func (c *Client) DecisionTimeout() time.Duration { return c.cfg.Timeout }

// QuestionType is a System One question type.
type QuestionType string

const (
	TypeChoice QuestionType = "choice"
	TypeScore  QuestionType = "score"
	TypeNoul   QuestionType = "noul"
)

// Question is one typed question. Build them with Choice, Score and Noul.
type Question struct {
	Type QuestionType `json:"type"`
	// Instructions is a string, or an object/array that puts the question in one
	// field and the data it refers to in others.
	Instructions any `json:"instructions"`
	// Criteria: for a Choice a map of option to description (nil = no
	// description); for a Score an ordered array of level descriptions; for a
	// Noul an optional {"true": ..., "false": ...} map.
	Criteria any `json:"criteria,omitempty"`
}

// Choice builds a Choice question. A nil description means the option needs no
// extra detail.
func Choice(instructions any, options map[string]any) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: options}
}

// Score builds a Score question over ordered levels.
func Score(instructions any, levels ...any) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Noul builds a yes/no question.
func Noul(instructions any) Question {
	return Question{Type: TypeNoul, Instructions: instructions}
}

// AskRequest is one System One call.
type AskRequest struct {
	// State is a string, or an object or array of text.
	State any
	// Questions are answered independently under the same keys.
	Questions map[string]Question
}

// Answer is one answer. Which fields are set depends on Type.
type Answer struct {
	Type QuestionType `json:"type"`
	// Choice (Type choice): the highest-probability option.
	Choice string `json:"choice,omitempty"`
	// Noul (Type noul): the probability of yes.
	Noul float64 `json:"noul,omitempty"`
	// Score (Type score): the probability-weighted level, and its legend.
	Score  float64           `json:"score,omitempty"`
	Legend map[string]string `json:"legend,omitempty"`
	// Probabilities (choice, score): option or level to probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is nil for a noul.
	Confidence *float64 `json:"confidence,omitempty"`
}

// AskResponse is the decoded answer to one call.
type AskResponse struct {
	// Model is the versioned model id that answered (for example "jev-1.13.0").
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

type wireRequest struct {
	State     json.RawMessage     `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// estimateTokens is the approximate token count of n bytes of JSON (bytes/4).
func estimateTokens(n int) int { return (n + 3) / 4 }

// encode checks req locally and builds the request body. Nothing in its errors
// quotes the request.
func (c *Client) encode(req AskRequest) ([]byte, error) {
	if len(req.Questions) > MaxQuestionsPerCall {
		return nil, fmt.Errorf("%w: %d (limit %d)", ErrTooManyQuestions, len(req.Questions), MaxQuestionsPerCall)
	}
	state, err := json.Marshal(req.State)
	if err != nil {
		return nil, fmt.Errorf("%w: the state cannot be encoded", ErrInvalidRequest)
	}
	longest := 0
	for _, q := range req.Questions {
		if opts, ok := q.Criteria.(map[string]any); ok && q.Type == TypeChoice && len(opts) > MaxChoiceOptions {
			return nil, fmt.Errorf("%w: %d (limit %d)", ErrTooManyOptions, len(opts), MaxChoiceOptions)
		}
		qb, err := json.Marshal(q)
		if err != nil {
			return nil, fmt.Errorf("%w: a question cannot be encoded", ErrInvalidRequest)
		}
		longest = max(longest, len(qb))
	}
	if limit := c.cfg.MaxStateTokens; limit > 0 {
		if est := estimateTokens(len(state) + longest); est > limit {
			return nil, fmt.Errorf("%w: about %d tokens (limit %d, estimate is approximate)", ErrStateTooLarge, est, limit)
		}
	}
	return json.Marshal(wireRequest{State: state, Model: c.cfg.Model, Questions: req.Questions})
}

// Ask makes one call. It does not retry.
func (c *Client) Ask(ctx context.Context, req AskRequest) (*AskResponse, error) {
	start := c.cfg.Clock()
	resp, status, err := c.do(ctx, req)
	if c.cfg.OnCall != nil {
		ev := CallEvent{Engine: c.cfg.Name, Questions: len(req.Questions), Latency: c.cfg.Clock().Sub(start), Status: status, Err: err}
		if resp != nil {
			ev.Model, ev.Usage = resp.Model, resp.Usage
		}
		c.cfg.OnCall(ev)
	}
	return resp, err
}

func (c *Client) do(ctx context.Context, req AskRequest) (*AskResponse, int, error) {
	body, err := c.encode(req)
	if err != nil {
		return nil, 0, err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + systemOnePath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("typesafe: build request: %w", err)
	}
	hreq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", "strongo-aichat")

	hresp, err := c.cfg.HTTPClient.Do(hreq)
	if err != nil {
		// A *url.Error carries the URL (no key: the key is only in a header).
		return nil, 0, fmt.Errorf("typesafe: request failed: %w", err)
	}
	defer func() { _ = hresp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, hresp.StatusCode, fmt.Errorf("typesafe: read response: %w", err)
	}

	if hresp.StatusCode < 200 || hresp.StatusCode > 299 {
		return nil, hresp.StatusCode, newAPIError(hresp, raw, c.cfg.Clock())
	}
	if int64(len(raw)) > c.cfg.MaxResponseBytes {
		return nil, hresp.StatusCode, fmt.Errorf("%w: response larger than %d bytes", ErrBadResponse, c.cfg.MaxResponseBytes)
	}
	var out AskResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		// Do not wrap err: a JSON syntax error can quote a fragment of the body.
		return nil, hresp.StatusCode, fmt.Errorf("%w: not JSON of the documented shape", ErrBadResponse)
	}
	if out.Answers == nil {
		return nil, hresp.StatusCode, fmt.Errorf("%w: no answers", ErrBadResponse)
	}
	return &out, hresp.StatusCode, nil
}

// newAPIError builds an APIError from a non-2xx response, keeping only the
// error type from the body. The live API answers {"detail": {"error_type": ...,
// "message": ...}} or {"detail": "<text>"}; the text is dropped because a
// validation message can echo submitted content.
func newAPIError(resp *http.Response, body []byte, now time.Time) *APIError {
	e := &APIError{
		Status:     resp.StatusCode,
		RequestID:  resp.Header.Get("x-typesafe-request-id"),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), now),
		kind:       kindForStatus(resp.StatusCode),
	}
	var shape struct {
		Detail struct {
			ErrorType string `json:"error_type"`
		} `json:"detail"`
	}
	// A string "detail" fails to decode into the struct; that is the "no
	// error type" case, so the decode error is deliberately ignored.
	_ = json.Unmarshal(body, &shape)
	e.ErrorType = shape.Detail.ErrorType
	return e
}
