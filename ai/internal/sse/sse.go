// Package sse is a tiny internal helper shared by every SSE reader in this
// module (ai/openaicompat, ai/anthropic, ai/cloudproto): a bufio.SplitFunc
// that behaves like bufio.ScanLines but also accepts a bare '\r' (old
// Mac-style line endings), which some SSE producers/proxies still emit.
package sse

import "bytes"

// ScanLines is bufio.ScanLines but also splits on a bare '\r', not just
// "\n" and "\r\n". Pass it to (*bufio.Scanner).Split.
//
// A '\r' found as the LAST byte of the current buffer is ambiguous when
// atEOF is false: more data may be coming, and the next byte could be the
// '\n' of a "\r\n" pair. In that case ScanLines asks bufio.Scanner for more
// data (returns 0, nil, nil) rather than guessing -- guessing wrong would
// split what is really one CRLF-terminated line into two, one of them a
// spurious blank line, right at a buffer boundary.
func ScanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	i := bytes.IndexAny(data, "\r\n")
	if i < 0 {
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
	if data[i] == '\n' {
		return i + 1, data[:i], nil
	}
	// data[i] == '\r'
	if i+1 < len(data) {
		if data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	}
	// '\r' is the last byte currently buffered: ambiguous unless this is
	// really the end of the stream.
	if !atEOF {
		return 0, nil, nil
	}
	return i + 1, data[:i], nil
}
