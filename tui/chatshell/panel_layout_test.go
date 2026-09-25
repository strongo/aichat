package chatshell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/strongo/aichat/ai/session"
	"github.com/strongo/aichat/tui/theme"
)

// bottomEdgeRow scans lines for the LAST row whose columns [from, to)
// contain glyph ("▀", the half-block bottom-edge marker both Card/
// ComposerFrame and the reworked PanelFrame share) -- a column-range
// search, not a whole-line one, so the composer's own bottom edge (which
// only ever occupies the LEFT/chat column) is never confused with the
// panel's (which only ever occupies the RIGHT/side column), even though
// both rows are scanned from the exact same rendered View() output.
func bottomEdgeRow(t *testing.T, lines []string, from, to int, glyph string) (row int, found bool) {
	t.Helper()
	row = -1
	for i, line := range lines {
		runes := []rune(ansi.Strip(line))
		lo, hi := from, to
		if hi > len(runes) {
			hi = len(runes)
		}
		if lo >= hi {
			continue
		}
		if strings.Contains(string(runes[lo:hi]), glyph) {
			row, found = i, true
		}
	}
	return row, found
}

// TestPanelBottomEdgeAlignsWithComposerBottomEdge covers the r12
// coordinator's final review, verbatim (measured on stripped ANSI dumps
// at 120x33): "the panel's bottom ▘▀▀ edge is on row 25 while the
// composer's bottom ▀ edge is on row 29 -- the panel stops 4 rows short
// ... panel bottom edge row == composer bottom edge row (the panel spans
// the transcript area AND the composer rows; the composer sits only in
// the left column)." Swept across chips on/off, a hints provider on/off,
// and the panel focused/unfocused -- none of which should change whether
// the two bottom edges land on the same row.
func TestPanelBottomEdgeAlignsWithComposerBottomEdge(t *testing.T) {
	for _, withChips := range []bool{false, true} {
		for _, withHints := range []bool{false, true} {
			for _, panelFocused := range []bool{false, true} {
				withTrueColorEnv(t, func() {
					var opts []Option
					if withHints {
						opts = append(opts, WithHintsProvider(func(int) ([]theme.Hint, []string) {
							return []theme.Hint{{Key: "Enter", Label: "send"}}, nil
						}))
					}
					h := &fakeHandler{}
					m := New(h, opts...)
					m.Update(tea.WindowSizeMsg{Width: 120, Height: 33})
					if withChips {
						m.SetChips([]Chip{{ID: "a", Label: "Customer"}})
					}
					m.PinToSidebar(session.EntityRef{Type: "t", Keys: map[string]string{"id": "1"}, Title: "Pinned"})
					m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
					if !panelFocused {
						m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
					}

					lines := strings.Split(m.View().Content, "\n")
					chatWidth := m.chatWidth()
					totalWidth := len([]rune(ansi.Strip(lines[0])))

					composerRow, ok := bottomEdgeRow(t, lines, 0, chatWidth, "▀")
					if !ok {
						t.Fatalf("chips=%v hints=%v focused=%v: no composer bottom edge found in the chat column:\n%s", withChips, withHints, panelFocused, ansi.Strip(m.View().Content))
					}
					panelRow, ok := bottomEdgeRow(t, lines, chatWidth, totalWidth, "▀")
					if !ok {
						t.Fatalf("chips=%v hints=%v focused=%v: no panel bottom edge found in the side column:\n%s", withChips, withHints, panelFocused, ansi.Strip(m.View().Content))
					}
					if composerRow != panelRow {
						t.Fatalf("chips=%v hints=%v focused=%v: composer bottom edge row=%d, panel bottom edge row=%d, want equal:\n%s", withChips, withHints, panelFocused, composerRow, panelRow, ansi.Strip(m.View().Content))
					}
				})
			}
		}
	}
}

// TestComposerIsLastScreenLineWithoutAStatusBar covers the r12
// coordinator's other requirement from the same review: "with no hints,
// the composer's bottom edge must be the last screen line (no trailing
// empty rows); with hints, the hints are the last line(s)." Checked both
// ways: no hints provider/status at all (the composer's own last
// rendered row must be the LAST line of View(), full stop), and with a
// hints provider (the hints text itself must be the last non-empty
// line).
func TestComposerIsLastScreenLineWithoutAStatusBar(t *testing.T) {
	t.Run("no status at all", func(t *testing.T) {
		h := &fakeHandler{}
		m := newTestShell(h)
		lines := strings.Split(m.View().Content, "\n")
		last := ansi.Strip(lines[len(lines)-1])
		if strings.TrimSpace(last) == "" {
			t.Fatalf("last screen line is blank, want the composer's own bottom edge as the terminal's literal last row:\n%s", ansi.Strip(m.View().Content))
		}
	})
	t.Run("with a hints provider", func(t *testing.T) {
		h := &fakeHandler{}
		m := New(h, WithHintsProvider(func(int) ([]theme.Hint, []string) {
			return []theme.Hint{{Key: "Enter", Label: "send"}}, nil
		}))
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		lines := strings.Split(m.View().Content, "\n")
		last := ansi.Strip(lines[len(lines)-1])
		if !strings.Contains(last, "Enter") || !strings.Contains(last, "send") {
			t.Fatalf("last screen line = %q, want it to be the hints line", last)
		}
	})
}

// TestStatusBarAlignsWithComposerSurfaceAndHasNoVerticalMargin covers the
// r14 founder ruling, verbatim: "Status panel should be aligned with
// composer border, not composer text and should have no vertical
// margins" -- superseding r12's card-text alignment and its blank margin
// row above the status bar. Checked with a hints provider active (the
// only way there IS a status line to measure): (a) its first
// non-blank column is the composer surface's own first column
// (composerMarkerWidth -- the column right after the marker, where the
// composer's own ▄/▀ edge begins), not the composer's TEXT column
// (theme.ComposerTextColumn(), which is further right); (b) the row
// immediately above the status line is the composer's own bottom ▀ edge
// -- no blank row between them; (c) the status line is the terminal's
// literal last row.
func TestStatusBarAlignsWithComposerSurfaceAndHasNoVerticalMargin(t *testing.T) {
	withTrueColorEnv(t, func() {
		h := &fakeHandler{}
		m := New(h, WithHintsProvider(func(int) ([]theme.Hint, []string) {
			return []theme.Hint{{Key: "Enter", Label: "send"}}, nil
		}))
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		lines := strings.Split(m.View().Content, "\n")

		last := ansi.Strip(lines[len(lines)-1])
		if !strings.Contains(last, "Enter") || !strings.Contains(last, "send") {
			t.Fatalf("last screen line = %q, want the status/hints line", last)
		}
		firstNonBlank := 0
		for i, r := range []rune(last) {
			if r != ' ' {
				firstNonBlank = i
				break
			}
		}
		if firstNonBlank != composerMarkerWidth {
			t.Fatalf("status line's first non-blank column = %d, want %d (the composer surface's own first column, right after the marker)", firstNonBlank, composerMarkerWidth)
		}
		if textCol := theme.ComposerTextColumn(); firstNonBlank == textCol {
			t.Fatalf("status line aligned to the composer TEXT column (%d), want the composer SURFACE column instead", textCol)
		}

		bottomEdgeRowIdx, ok := bottomEdgeRow(t, lines[:len(lines)-1], 0, m.chatWidth(), "▀")
		if !ok {
			t.Fatalf("no composer bottom edge found above the status line:\n%s", ansi.Strip(m.View().Content))
		}
		if bottomEdgeRowIdx != len(lines)-2 {
			t.Fatalf("composer bottom edge is on row %d, want row %d (directly above the status line, row %d, with no blank row between them)", bottomEdgeRowIdx, len(lines)-2, len(lines)-1)
		}
	})
}
