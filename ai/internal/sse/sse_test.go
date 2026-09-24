package sse

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func scanAll(t *testing.T, chunks []string) []string {
	t.Helper()
	r, w := io.Pipe()
	go func() {
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
		}
		_ = w.Close()
	}()
	sc := bufio.NewScanner(r)
	sc.Split(ScanLines)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return lines
}

func TestScanLines_LF(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\nb\nc"))
	sc.Split(ScanLines)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanLines_CRLF(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\r\nb\r\nc"))
	sc.Split(ScanLines)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanLines_BareCR(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\rb\rc"))
	sc.Split(ScanLines)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanLines_CRLFSplitAcrossBufferBoundary(t *testing.T) {
	// The '\r' arrives as the very last byte of one read, and the '\n'
	// arrives at the start of the next: a naive scanner treats the lone
	// '\r' as a bare-CR line ending immediately, producing a spurious extra
	// blank line for the '\n' that follows. ScanLines must instead wait for
	// more data and recognise the pair as one CRLF.
	got := scanAll(t, []string{"line-one\r", "\nline-two\r\n"})
	want := []string{"line-one", "line-two"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanLines_BareCRAtEOF(t *testing.T) {
	// A trailing bare '\r' with nothing after it and no more data coming
	// (atEOF) must still be recognised as a line ending, not held forever.
	sc := bufio.NewScanner(strings.NewReader("only-line\r"))
	sc.Split(ScanLines)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if len(got) != 1 || got[0] != "only-line" {
		t.Fatalf("got = %v, want [\"only-line\"]", got)
	}
}

func TestScanLines_NoTrailingNewline(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\nb\nc-no-newline"))
	sc.Split(ScanLines)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	want := []string{"a", "b", "c-no-newline"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
}

func TestScanLines_Empty(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader(""))
	sc.Split(ScanLines)
	if sc.Scan() {
		t.Fatalf("expected no lines from empty input, got %q", sc.Text())
	}
}
