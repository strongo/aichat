// Package stream pumps an ai.LLMProvider's iter.Seq2[ai.Event, error] into
// Bubble Tea messages without buffering the response: each event arrives as
// an EventMsg carrying the tea.Cmd that re-arms the pump for the next event
// (the "channel re-arm" pattern DataTug chat used for HTTP/LLM streaming).
// The final message is always a DoneMsg, whether the stream completed,
// errored, or ctx was cancelled.
package stream
