package typesafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// testKey is a fake key used only against fakes; it is not a credential.
const testKey = "test-key-not-a-secret"

// testModel is the pinned model id the tests configure.
const testModel = "jev-1.13.0"

// fakeDoer is a canned HTTP exchange that records the request.
type fakeDoer struct {
	status  int
	headers map[string]string
	body    string
	err     error
	readErr error // body reader fails with this

	calls   int
	req     *http.Request
	reqBody []byte
}

func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	f.calls++
	f.req = r
	if r.Body != nil {
		f.reqBody, _ = io.ReadAll(r.Body)
	}
	if f.err != nil {
		return nil, f.err
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range f.headers {
		h.Set(k, v)
	}
	status := f.status
	if status == 0 {
		status = 200
	}
	var body io.Reader = bytes.NewReader([]byte(f.body))
	if f.readErr != nil {
		body = errReader{f.readErr}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(body)}, nil
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// sent decodes the recorded request body.
func (f *fakeDoer) sent(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.reqBody, &m); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, f.reqBody)
	}
	return m
}

func (f *fakeDoer) questions(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for k, v := range f.sent(t)["questions"].(map[string]any) {
		out[k] = v.(map[string]any)
	}
	return out
}

func newClient(t *testing.T, d *fakeDoer, mutate ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{APIKey: testKey, Model: testModel, HTTPClient: d}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeClock returns successive times 10ms apart.
func steppingClock() func() time.Time {
	t := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		cur := t
		t = t.Add(10 * time.Millisecond)
		return cur
	}
}

var errNetwork = errors.New("connection reset")

func jsonFloat(f float64) (string, error) {
	b, err := json.Marshal(f)
	return string(b), err
}
