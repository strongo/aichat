package grid

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/tui/theme"
)

// TestTruncatedCellsEndWithEllipsis is the r11 grid-truncation guard
// (coordinator, verbatim, relaying a founder report from Warp: "the
// grid's Name column shows 'Customer' for 'Customer 1' -- cells
// truncated to the column width are cut silently. Truncated cell text
// must end with '…' ... in data cells and headers, both themes"):
// whenever a header or data cell's rendered text is narrower than its
// raw source text -- i.e. it was actually clipped to fit the column --
// the rendered text must end with "…", never a silently cut word.
//
// This exercises the REAL rendering path (grid.Model.View(), the same
// bubble-table-backed table both aichat's own chatshell demos and
// DataTug's grid.go adapter render through) across a sweep of narrow
// widths and both theme.Dark variants, rather than asserting against the
// underlying library's own truncation helper directly -- so a future
// regression in how aichat SIZES columns (not just how it truncates
// them) would also be caught here.
func TestTruncatedCellsEndWithEllipsis(t *testing.T) {
	prevDark := theme.Dark
	t.Cleanup(func() { theme.SetDark(prevDark) })

	const header = "Customer Name"
	const value = "Customer 1234567890"
	cols := []Column{{Name: header}}
	rows := []Row{{Values: []any{value}}}

	for _, dark := range []bool{true, false} {
		theme.SetDark(dark)
		for width := 6; width <= 24; width++ {
			t.Run(fmt.Sprintf("dark=%v/width=%d", dark, width), func(t *testing.T) {
				m := New(cols, rows)
				m.Update(tea.WindowSizeMsg{Width: width, Height: 10})
				view := m.View(width, true)
				lines := strings.Split(view, "\n")
				if len(lines) < 3 {
					t.Fatalf("expected at least a header + one data row: %q", view)
				}
				assertCellNotSilentlyCut(t, "header", header, lines[1])
				assertCellNotSilentlyCut(t, "data", value, lines[2])
			})
		}
	}
}

// firstCellFrom extracts a single-column table's cell text from one
// rendered border-delimited row, e.g. "│Customer…     ┃│" -> "Customer…
// " (still right-padded; assertCellNotSilentlyCut trims that).
// bubble-table always emits a "┃" divider after even a single (and
// therefore also final) column, immediately before the table's own
// outer-right "│" -- see the fixture's own probed output.
func firstCellFrom(line string) string {
	plain := ansi.Strip(line)
	if i := strings.Index(plain, "┃"); i >= 0 {
		plain = plain[:i]
	}
	return strings.TrimPrefix(plain, "│")
}

// assertCellNotSilentlyCut fails when renderedLine's cell text is
// shorter than raw (i.e. it was truncated) but doesn't end with "…".
// Empty (too narrow to render anything) is not itself a failure --
// there's no silently-cut WORD to complain about, only a floor bubble-
// table itself already handles (limitStr returns "" outright once
// maxLen reaches 0).
func assertCellNotSilentlyCut(t *testing.T, label, raw, renderedLine string) {
	t.Helper()
	cell := strings.TrimRight(firstCellFrom(renderedLine), " ")
	if cell == "" || cell == raw {
		return
	}
	if !strings.HasSuffix(cell, "…") {
		t.Fatalf("%s cell %q was clipped from %q without an ellipsis (line: %q)", label, cell, raw, renderedLine)
	}
}
