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
//     network.
//   - There are no retries here. Failing over to another engine, and not hammering
//     an engine that is down, belong to the compose combinators and breaker.
//   - Nothing is logged. Error values carry a status, an error type and a request
//     id, never the response body or the submitted state. The API key is only ever
//     placed in the Authorization header and is redacted from String().
//   - Answers are calibrated probabilities, so the decision types it returns have
//     Calibrated set.
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
	"net/http"
	"strings"
	"time"

	"github.com/strongo/aichat/ai/decision"
)

// Defaults and API limits (from the published model documentation).
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultModel is the alias of the most recent stable release. Pin a
	// versioned id (for example "jev-1.13.0") to keep answers reproducible.
	DefaultModel = "jev-latest"
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
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// Model defaults to DefaultModel.
	Model string
	// Name is the engine's name in decision.Trace (default DefaultName).
	Name string
	// HTTPClient defaults to a plain *http.Client (no client-side timeout: the
	// context bounds each call).
	HTTPClient HTTPDoer
	// Timeout is reported by DecisionTimeout (default DefaultTimeout).
	Timeout time.Duration
	// MaxResponseBytes bounds how much of a response is read.
	MaxResponseBytes int64
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

// New builds a Client. It fails only when APIKey is empty.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("typesafe: APIKey is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
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
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
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
	body, err := json.Marshal(wireRequest{State: req.State, Model: c.cfg.Model, Questions: req.Questions})
	if err != nil {
		return nil, 0, fmt.Errorf("typesafe: encode request: %w", err)
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
		return nil, hresp.StatusCode, newAPIError(hresp, raw)
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
func newAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{
		Status:     resp.StatusCode,
		RequestID:  resp.Header.Get("x-typesafe-request-id"),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
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
