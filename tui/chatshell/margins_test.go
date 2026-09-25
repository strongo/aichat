package chatshell

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fillSidePanel is a SidePanel test double whose View fills its entire
// given width/height with a fixed, unstyled rune -- unlike fakeSidePanel's
// literal "sidepanel" placeholder, this lets a test locate exactly which
// screen columns the panel's own content actually reached, to check it
// against theme's outer-margin/frame math.
type fillSidePanel struct{ rune rune }

func (p fillSidePanel) Title() string { return "fill" }

func (p fillSidePanel) View(width, height int, focused bool) string {
	row := strings.Repeat(string(p.rune), max(1, width))
	rows := make([]string, max(1, height))
	for i := range rows {
		rows[i] = row
	}
	return strings.Join(rows, "\n")
}

func (p fillSidePanel) Update(msg tea.Msg) (SidePanel, tea.Cmd) { return p, nil }

// TestChatAndPanelWidthBudgetLeavesSymmetricOneColumnMargin covers founder
// 2026-09-25's "Why there is right margin on right of the screen?" fix
// (coordinator measured a 2-column right margin against the chat column's
// own 1-column left margin, at 120 columns with the panel shown): the
// total screen-row budget chatWidth()/sidebarWidth() split between them
// must always be EXACTLY m.width-1 -- the same single reserved column a
// card's own blank focus-marker column already gives the chat column's
// left edge -- whether or not the side panel is visible, at every width
// this package treats specially (below/at/above splitMinWidth).
func TestChatAndPanelWidthBudgetLeavesSymmetricOneColumnMargin(t *testing.T) {
	for _, width := range []int{80, 110, 120} {
		for _, withPanel := range []bool{false, true} {
			h := &fakeHandler{}
			panel := &fakeSidePanel{title: "workspace"}
			var m *Model
			if withPanel {
				m = New(h, WithSidePanel(panel))
			} else {
				m = New(h)
			}
			m.Update(tea.WindowSizeMsg{Width: width, Height: 30})

			total := m.chatWidth()
			if m.splitEnabled() {
				total += m.sidebarWidth()
			}
			if want := max(1, m.width-1); total != want {
				t.Errorf("width=%d withPanel=%v: chat+panel budget = %d, want %d (m.width-1)", width, withPanel, total, want)
			}
		}
	}
}

// lastResetSuffixLen returns how many raw bytes follow the LAST ANSI SGR
// reset ("\x1b[m"/"\x1b[0m") in line -- for a row that is one continuous
// styled surface followed by nothing but the terminal's own default
// background, that suffix is exactly the blank OUTER margin past the
// surface's own right edge (every styled span this package renders closes
// with a reset via paintOver/lipgloss, so a reset with only plain
// characters after it, and no further escape codes, marks where the
// surface's own paint stops for good).
func lastResetSuffixLen(line string) int {
	idx := strings.LastIndex(line, "\x1b[m")
	resetLen := len("\x1b[m")
	if alt := strings.LastIndex(line, "\x1b[0m"); alt > idx {
		idx, resetLen = alt, len("\x1b[0m")
	}
	if idx < 0 {
		return -1
	}
	suffix := line[idx+resetLen:]
	if strings.Contains(suffix, "\x1b[") {
		return -1 // more escape codes follow -- not the row's final reset
	}
	return len([]rune(suffix))
}

// TestOuterRightMarginMatchesLeftMargin renders a full View() at a
// split-eligible width with a SidePanel that fills every column it's
// given (fillSidePanel), then checks the actual rendered row: exactly ONE
// raw, unstyled screen column must remain past the panel surface's own
// last painted column -- the same single blank column the chat card's own
// left focus marker leaves on the left (surfaceFill's doc) -- covering
// founder 2026-09-25's "Why there is right margin on right of the
// screen?" (coordinator measured 2, not 1, at 120 columns with the panel
// shown).
func TestOuterRightMarginMatchesLeftMargin(t *testing.T) {
	const width = 120
	h := &fakeHandler{}
	panel := fillSidePanel{rune: 'X'}
	m := New(h, WithSidePanel(panel))
	m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	if !m.splitEnabled() {
		t.Fatal("expected split to be enabled at width 120")
	}

	lines := strings.Split(m.View().Content, "\n")
	var checked bool
	for _, line := range lines {
		if !strings.Contains(line, "X") {
			continue // a row the panel's own content didn't reach (blank filler row)
		}
		if ansi.StringWidth(line) != width {
			continue
		}
		margin := lastResetSuffixLen(line)
		if margin < 0 {
			t.Fatalf("row %q: could not locate the surface's own final reset", line)
		}
		if margin != 1 {
			t.Fatalf("row %q: %d raw columns past the panel surface's own edge, want 1", line, margin)
		}
		checked = true
	}
	if !checked {
		t.Fatal("no full-width rendered row contained the panel's own fill rune -- test fixture problem")
	}
}
