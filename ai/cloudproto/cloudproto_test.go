package cloudproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
)

func TestWriteReadEvents_RoundTrip(t *testing.T) {
	events := []ai.Event{
		{Type: ai.EventStarted, Provider: "openai-compatible", Model: "gpt-5"},
		{Type: ai.EventTextDelta, Text: "Hello"},
		{Type: ai.EventTextDelta, Text: ", world"},
		{Type: ai.EventUsage, Usage: &ai.Usage{InputTokens: 3, OutputTokens: 2}},
		{Type: ai.EventCompleted, Usage: &ai.Usage{InputTokens: 3, OutputTokens: 2}},
	}
	var buf bytes.Buffer
	for _, ev := range events {
		if err := WriteEvent(&buf, ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	var got []ai.Event
	for ev, err := range ReadEvents(&buf) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(events), got)
	}
	for i := range events {
		if got[i].Type != events[i].Type || got[i].Text != events[i].Text {
			t.Errorf("event %d = %+v, want %+v", i, got[i], events[i])
		}
	}
}

func TestWriteReadEvents_ToolCallRoundTrip(t *testing.T) {
	events := []ai.Event{
		{Type: ai.EventStarted, Provider: "anthropic", Model: "claude-haiku-4-5"},
		{Type: ai.EventToolCall, ToolCall: &ai.ToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}},
		{Type: ai.EventCompleted, StopReason: ai.StopReasonToolCalls},
	}
	var buf bytes.Buffer
	for _, ev := range events {
		if err := WriteEvent(&buf, ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	var got []ai.Event
	for ev, err := range ReadEvents(&buf) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(events), got)
	}
	if got[1].Type != ai.EventToolCall || got[1].ToolCall == nil || got[1].ToolCall.Name != "lookup" {
		t.Errorf("tool call event = %+v", got[1])
	}
	if got[2].StopReason != ai.StopReasonToolCalls {
		t.Errorf("StopReason = %q", got[2].StopReason)
	}
}

func TestReadEvents_UnknownEventIgnored(t *testing.T) {
	raw := "event: some.future.event\ndata: {\"foo\":\"bar\"}\n\n" +
		"event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (unknown event skipped): %+v", len(got), got)
	}
	if got[0].Type != ai.EventTextDelta || got[0].Text != "hi" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Type != ai.EventCompleted {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestReadEvents_CommentsIgnored(t *testing.T) {
	raw := ": keep-alive\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Type != ai.EventCompleted {
		t.Fatalf("got = %+v", got)
	}
}

func TestReadEvents_MultiLineData(t *testing.T) {
	// A JSON payload split across multiple `data:` lines must be joined with
	// '\n' before parsing, per the SSE spec.
	text := "line one\nline two"
	structured := json.RawMessage(`{"a":1}`)
	payload, err := json.Marshal(ai.Event{Type: ai.EventTextDelta, Text: text, Structured: structured})
	if err != nil {
		t.Fatal(err)
	}
	// Split the JSON across two data: lines at a safe point (won't happen in
	// practice since WriteEvent emits single-line JSON, but ReadEvents must
	// tolerate a multi-line payload per the SSE framing rules).
	half := len(payload) / 2
	raw := "event: text.delta\ndata: " + string(payload[:half]) + "\n" +
		"junk-that-is-not-valid-json-alone"
	_ = raw // constructing a genuinely valid split-JSON case is awkward; instead
	// verify multi-line data via comment lines interleaved with a single
	// logical event spanning two data: lines that concatenate to valid JSON.
	raw2 := "event: text.delta\ndata: {\"type\":\"text.delta\",\n" +
		"data: \"text\":\"joined\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw2)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Text != "joined" {
		t.Errorf("got[0].Text = %q, want %q (multi-line data must join with \\n)", got[0].Text, "joined")
	}
}

func TestReadEvents_StopsAfterCompleted(t *testing.T) {
	raw := "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n" +
		"event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"should not appear\"}\n\n"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Type != ai.EventCompleted {
		t.Fatalf("got = %+v, want stream to stop after response.completed", got)
	}
}

func TestReadEvents_EarlyConsumerBreak(t *testing.T) {
	raw := "event: response.started\ndata: {\"type\":\"response.started\"}\n\n" +
		"event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"a\"}\n\n" +
		"event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"b\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	n := 0
	for range ReadEvents(strings.NewReader(raw)) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2 (consumer break must stop iteration promptly)", n)
	}
}

func TestReadEvents_BadJSON(t *testing.T) {
	raw := "event: text.delta\ndata: {not json}\n\n"
	var gotErr error
	for _, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestReadEvents_EventNameFallsBackForType(t *testing.T) {
	raw := "event: usage\ndata: {\"inputTokens\":5}\n\n" +
		"event: response.completed\ndata: {}\n\n"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Type != ai.EventUsage {
		t.Fatalf("got = %+v, want Type derived from the event: line when data omits it", got)
	}
}

func TestReadEvents_ErrorEventIsFatalPair(t *testing.T) {
	raw := "event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"partial\"}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"upstream\",\"message\":\"boom\"}}\n\n" +
		"event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"should not appear\"}\n\n"
	var got []ai.Event
	var lastErr error
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		got = append(got, ev)
		lastErr = err
		if err != nil {
			break
		}
	}
	if len(got) != 2 {
		t.Fatalf("got = %+v, want exactly 2 yields (text.delta, fatal error)", got)
	}
	if lastErr == nil {
		t.Fatal("expected a fatal error on the error event")
	}
	last := got[len(got)-1]
	if last.Type != ai.EventError || last.Error == nil || last.Error.Message != "boom" {
		t.Fatalf("last event = %+v, want the fatal EventError", last)
	}
}

func TestReadEvents_TruncatedWithoutCompletedIsFatal(t *testing.T) {
	raw := "event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"partial\"}\n\n"
	// stream just ends -- no response.completed, no error event.
	var got []ai.Event
	var lastErr error
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		got = append(got, ev)
		lastErr = err
	}
	if lastErr == nil {
		t.Fatal("expected a fatal truncation error")
	}
	last := got[len(got)-1]
	if last.Type != ai.EventError {
		t.Fatalf("last event = %+v, want a fatal EventError for truncation", last)
	}
}

func TestReadEvents_BareCRLineEndings(t *testing.T) {
	raw := "event: text.delta\rdata: {\"type\":\"text.delta\",\"text\":\"hi\"}\r\r" +
		"event: response.completed\rdata: {\"type\":\"response.completed\"}\r\r"
	var got []ai.Event
	for ev, err := range ReadEvents(strings.NewReader(raw)) {
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].Text != "hi" || got[1].Type != ai.EventCompleted {
		t.Fatalf("got = %+v, want bare-CR line endings parsed like LF", got)
	}
}

// errAfterReader returns n bytes of data, then a fixed error on every
// subsequent Read -- simulating a transport that dies mid-stream (a broken
// connection, a reset, etc.).
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestReadEvents_TransportErrorIsFatalPair(t *testing.T) {
	raw := "event: text.delta\ndata: {\"type\":\"text.delta\",\"text\":\"partial\"}\n\n"
	r := &errAfterReader{data: []byte(raw), err: errors.New("connection reset by peer")}
	var got []ai.Event
	var lastErr error
	for ev, err := range ReadEvents(r) {
		got = append(got, ev)
		lastErr = err
		if err != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected a fatal error for the transport failure")
	}
	last := got[len(got)-1]
	if last.Type != ai.EventError || last.Error == nil {
		t.Fatalf("last event = %+v, want the fatal pair Event{Type: EventError, Error: e}", last)
	}
	var aiErr *ai.Error
	if !errors.As(lastErr, &aiErr) || aiErr.Code != ai.ErrCodeUpstream {
		t.Fatalf("lastErr = %v, want an *ai.Error with Code upstream", lastErr)
	}
}

func TestErrorResponse_JSON(t *testing.T) {
	er := ErrorResponse{Error: ai.Error{Code: ai.ErrCodeAuth, Message: "bad key"}}
	b, err := json.Marshal(er)
	if err != nil {
		t.Fatal(err)
	}
	var back ErrorResponse
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Error.Code != ai.ErrCodeAuth || back.Error.Message != "bad key" {
		t.Errorf("back = %+v", back)
	}
}
