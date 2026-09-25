package theme

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// plain strips ANSI styling so content assertions don't need to reason
// about exactly which escape codes/spans wrap a given substring.
func plain(s string) string { return ansi.Strip(s) }

func withDark(t *testing.T, dark bool, fn func()) {
	t.Helper()
	prev := Dark
	SetDark(dark)
	t.Cleanup(func() { SetDark(prev) })
	fn()
}

func TestDetectNeverPanics(t *testing.T) {
	// os.Stdin/os.Stdout are almost certainly not a real terminal under `go
	// test`; Detect must still return a bool, not panic.
	got := Detect()
	if got != true && got != false {
		t.Fatalf("Detect returned non-bool-ish %v", got)
	}
}

func TestDetectRecoversFromPanic(t *testing.T) {
	prev := detectBackground
	detectBackground = func() bool { panic("exotic terminal blew up") }
	t.Cleanup(func() { detectBackground = prev })
	if got := Detect(); got != true {
		t.Fatalf("Detect() after panic = %v, want true (the safe default)", got)
	}
}

func TestCardRoundTripsBothVariants(t *testing.T) {
	for _, dark := range []bool{true, false} {
		withDark(t, dark, func() {
			for _, role := range []Role{RoleUser, RoleAssistant, RoleSystem, RoleError, RoleBlock, Role("unknown")} {
				out := Card(role, HeaderFor(role), "hello world", 40, false)
				if !strings.Contains(plain(out), "hello world") {
					t.Fatalf("dark=%v role=%s: card missing body: %q", dark, role, out)
				}
				focused := Card(role, HeaderFor(role), "hello world", 40, true)
				if focused == out {
					t.Fatalf("dark=%v role=%s: focused card identical to unfocused", dark, role)
				}
			}
		})
	}
}

func TestCardWithoutHeader(t *testing.T) {
	out := Card(RoleBlock, "", "body only", 30, false)
	if strings.Contains(out, "\n\nbody only") {
		t.Fatalf("unexpected blank header line: %q", out)
	}
	if !strings.Contains(plain(out), "body only") {
		t.Fatalf("missing body: %q", out)
	}
}

func TestInnerWidthNeverBelowOne(t *testing.T) {
	if got := InnerWidth(0); got != 1 {
		t.Fatalf("InnerWidth(0) = %d, want 1", got)
	}
	if got := InnerWidth(-5); got != 1 {
		t.Fatalf("InnerWidth(-5) = %d, want 1", got)
	}
	if got := InnerWidth(10); got != 10-cardBarWidth-2*CardPaddingCols {
		t.Fatalf("InnerWidth(10) = %d", got)
	}
}

func TestBarTruncatesAndFillsWidth(t *testing.T) {
	out := Bar(10, "this content is definitely longer than ten columns")
	if lipgloss.Width(out) != 10 {
		t.Fatalf("Bar width = %d, want 10; out=%q", lipgloss.Width(out), out)
	}
	// Zero/negative width must not panic and still returns something.
	_ = Bar(0, "x")
	_ = Bar(-3, "x")
}

func TestTopBarComposesTitleContextItems(t *testing.T) {
	out := TopBar(80, "DataTug", "Project: Foo", []MenuItem{{Label: "Sessions", Active: true}, {Label: "Help"}})
	if !strings.Contains(plain(out), "DataTug") || !strings.Contains(plain(out), "Project: Foo") || !strings.Contains(plain(out), "Sessions") || !strings.Contains(plain(out), "Help") {
		t.Fatalf("TopBar missing expected content: %q", out)
	}
}

func TestTopBarEmptyTitleAndContext(t *testing.T) {
	out := TopBar(40, "", "", nil)
	if lipgloss.Width(out) != 40 {
		t.Fatalf("width = %d, want 40", lipgloss.Width(out))
	}
}

func TestRenderHintsWithAndWithoutSegments(t *testing.T) {
	hints := []Hint{{Key: "Enter", Label: "send"}, {Key: "F6", Label: "workspace"}}
	out := RenderHints(80, hints)
	if !strings.Contains(plain(out), "Enter") || !strings.Contains(plain(out), "send") {
		t.Fatalf("missing hint content: %q", out)
	}
	withSeg := RenderHints(80, hints, "session summary")
	if !strings.Contains(plain(withSeg), "session summary") {
		t.Fatalf("missing segment content: %q", withSeg)
	}
	segOnly := RenderHints(80, nil, "only segment")
	if !strings.Contains(plain(segOnly), "only segment") {
		t.Fatalf("missing segment-only content: %q", segOnly)
	}
	empty := RenderHints(20, nil)
	if lipgloss.Width(empty) != 20 {
		t.Fatalf("empty hints width = %d, want 20", lipgloss.Width(empty))
	}
}

func TestRenderHintsWrapsOntoMultipleLinesWhenTooNarrow(t *testing.T) {
	hints := []Hint{
		{Key: "Shift+↑↓", Label: "navigate"}, {Key: "Enter", Label: "send"},
		{Key: "F6/Shift+→", Label: "workspace"}, {Key: "Ctrl+←→", Label: "resize"},
		{Key: "F3", Label: "projects"}, {Key: "F4", Label: "sessions"}, {Key: "Ctrl+C", Label: "quit"},
	}
	out := RenderHints(24, hints, "session summary line")
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected RenderHints to wrap onto multiple lines at width 24, got 1:\n%s", out)
	}
	flat := plain(out)
	for _, want := range []string{"session summary line", "navigate", "send", "quit"} {
		if !strings.Contains(flat, want) {
			t.Errorf("wrapped hints dropped %q:\n%s", want, flat)
		}
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w != 24 {
			t.Errorf("wrapped line %d width = %d, want 24: %q", i, w, line)
		}
	}
}

func TestComposerFrameFocusedVsUnfocused(t *testing.T) {
	unfocused := ComposerFrame(30, "type here", false)
	focused := ComposerFrame(30, "type here", true)
	if unfocused == focused {
		t.Fatal("focused composer frame identical to unfocused")
	}
	if !strings.Contains(plain(unfocused), "type here") || !strings.Contains(plain(focused), "type here") {
		t.Fatalf("frame missing content: unfocused=%q focused=%q", unfocused, focused)
	}
}

func TestComposerFrameSize(t *testing.T) {
	cols, rows := ComposerFrameSize()
	if cols != 5 || rows != 2 {
		t.Fatalf("ComposerFrameSize() = (%d, %d), want (5, 2) -- 1-col accent bar + 2 cols padding each side, 1 row padding above/below", cols, rows)
	}
}

// TestComposerFrameSurvivesNestedResetInContent covers the real regression
// this round: a bubbles textinput's own View() carries its own ANSI
// styling, including its own bare resets (its cursor cell, its placeholder
// styling) — before paintOver, that reset cut the composer's filled
// background off partway through the line, so the composer rendered on
// the bare terminal background instead of its themed fill ("lost composer
// background"). Reproduce the same shape (styled fragment + reset + more
// text) and assert the fill's background SGR is still present AFTER that
// embedded reset, not just at the very start of the line.
func TestComposerFrameSurvivesNestedResetInContent(t *testing.T) {
	nested := "\x1b[37m> \x1b[m\x1b[7;37mA\x1b[msk anything..."
	out := ComposerFrame(40, nested, false)
	bg, fg := SurfaceColors()
	want := fillSGR(bg, fg)
	// The nested fragment embeds two of its own bare resets ("\x1b[m")
	// before its final visible text ("sk anything..."). Before paintOver,
	// the fill's background SGR appeared only ONCE, at the very start of
	// the line -- after the first embedded reset, the rest of the line
	// (including "sk anything...") rendered on the bare terminal
	// background, not the fill (the real "lost composer background"
	// regression). paintOver reasserts the fill after every reset, so it
	// must now appear MORE THAN ONCE, with at least one occurrence AFTER
	// the nested content's own last reset -- i.e. still in effect for the
	// text that follows it.
	if strings.Count(out, want) < 2 {
		t.Fatalf("composer fill was not reasserted after the nested content's embedded reset (want the fill SGR %q to appear more than once): %q", want, out)
	}
	lastReset := strings.LastIndex(out, "\x1b[7;37mA\x1b[m")
	if lastReset < 0 {
		t.Fatalf("nested fragment not found verbatim in output: %q", out)
	}
	after := out[lastReset:]
	if !strings.Contains(after, want) {
		t.Fatalf("composer fill not reasserted after the nested content's cursor-cell reset: %q", after)
	}
}

func TestPanelFrameFocusedVsUnfocused(t *testing.T) {
	unfocused := PanelFrame(30, "panel content", false)
	focused := PanelFrame(30, "panel content", true)
	if unfocused == focused {
		t.Fatal("focused panel frame identical to unfocused")
	}
	if !strings.Contains(plain(unfocused), "panel content") || !strings.Contains(plain(focused), "panel content") {
		t.Fatalf("frame missing content: unfocused=%q focused=%q", unfocused, focused)
	}
}

func TestPanelFrameSize(t *testing.T) {
	cols, rows := PanelFrameSize()
	if cols != 1 || rows != 0 {
		t.Fatalf("PanelFrameSize() = (%d, %d), want (1, 0)", cols, rows)
	}
}

// TestPanelFrameDividerIsASingleUnfilledGlyph covers a real regression:
// PanelFrame used to reuse addLeftBar (a BACKGROUND-filled accent column,
// right for a card/composer's focus accent) for the transcript/panel
// divider too, which -- painted over the panel's own already-coloured row
// backgrounds -- read as "a wide striped grey band", not a clean divider.
// Founder correction: exactly one "│" glyph per line, coloured by
// FOREGROUND only (BorderColor(focused)), no background SGR at all.
func TestPanelFrameDividerIsASingleUnfilledGlyph(t *testing.T) {
	for _, focused := range []bool{false, true} {
		out := PanelFrame(30, "row one\nrow two", focused)
		for i, line := range strings.Split(out, "\n") {
			if got := strings.Count(ansi.Strip(line), "│"); got != 1 {
				t.Fatalf("focused=%v line %d has %d '│' glyphs, want exactly 1: %q", focused, i, got, line)
			}
			if strings.Contains(line, "\x1b[4") { // any SGR background parameter (38..48 family starts with "4" for bg in both 256-colour "48;5;" and truecolor "48;2;")
				t.Fatalf("focused=%v line %d carries a background SGR on the divider, want none: %q", focused, i, line)
			}
		}
	}
}

func TestSelectedRow(t *testing.T) {
	sel := SelectedRow("item", true)
	unsel := SelectedRow("item", false)
	if sel == unsel {
		t.Fatal("selected row identical to unselected")
	}
	if !strings.Contains(plain(sel), "item") || !strings.Contains(plain(unsel), "item") {
		t.Fatalf("row missing content: sel=%q unsel=%q", sel, unsel)
	}
	if !strings.HasPrefix(unsel, "  ") {
		t.Fatalf("unselected row should be indented: %q", unsel)
	}
}

func TestPanelHeader(t *testing.T) {
	if out := PanelHeader("Sidebar"); !strings.Contains(plain(out), "Sidebar") {
		t.Fatalf("PanelHeader missing text: %q", out)
	}
}

func TestHeaderForEveryRole(t *testing.T) {
	cases := map[Role]string{
		RoleUser:      "You",
		RoleAssistant: "Assistant",
		RoleSystem:    "System",
		RoleError:     "Error",
		RoleBlock:     "",
		Role("other"): "",
	}
	for role, want := range cases {
		if got := HeaderFor(role); got != want {
			t.Fatalf("HeaderFor(%s) = %q, want %q", role, got, want)
		}
	}
}

func TestSetDarkAndPickBothBranches(t *testing.T) {
	withDark(t, true, func() {
		if pick(fakeColor("light"), fakeColor("dark")) != fakeColor("dark") {
			t.Fatal("pick should choose dark branch when Dark=true")
		}
	})
	withDark(t, false, func() {
		if pick(fakeColor("light"), fakeColor("dark")) != fakeColor("light") {
			t.Fatal("pick should choose light branch when Dark=false")
		}
	})
}

// color is a tiny local helper so TestSetDarkAndPickBothBranches can compare
// pick's generic color.Color return without importing image/color's
// interface machinery directly in the test.
type fakeColor string

func (c fakeColor) RGBA() (r, g, b, a uint32) { return 0, 0, 0, 0 }

func TestContrastMeetsWCAG(t *testing.T) {
	prevDark := Dark
	t.Cleanup(func() { SetDark(prevDark) })
	for _, dark := range []bool{true, false} {
		SetDark(dark)
		variant := "dark"
		if !dark {
			variant = "light"
		}
		pairs := ContrastPairs()
		if len(pairs) == 0 {
			t.Fatalf("[%s] ContrastPairs returned none", variant)
		}
		for _, p := range pairs {
			ratio := Contrast(p.FG, p.BG)
			if ratio < p.MinimumRatio {
				t.Errorf("[%s] %s: contrast %.2f:1 below minimum %.2f:1 (fg=%#v bg=%#v)", variant, p.Name, ratio, p.MinimumRatio, p.FG, p.BG)
			}
		}
	}
}

// TestSurfaceDistinctFromTerminalBackground covers the r9 coordinator
// regression: a card or composer fill that's technically WCAG-compliant
// for its own TEXT can still be visually indistinguishable from the bare
// terminal background as a FILL (founder: "the assistant card fill is
// indistinguishable from the terminal background" in dark mode). Every
// SurfaceDeltaPairs() entry must clear minSurfaceDelta against
// TerminalBackground(), in both Dark variants.
func TestSurfaceDistinctFromTerminalBackground(t *testing.T) {
	prevDark := Dark
	t.Cleanup(func() { SetDark(prevDark) })
	for _, dark := range []bool{true, false} {
		SetDark(dark)
		variant := "dark"
		if !dark {
			variant = "light"
		}
		pairs := SurfaceDeltaPairs()
		if len(pairs) == 0 {
			t.Fatalf("[%s] SurfaceDeltaPairs returned none", variant)
		}
		term := TerminalBackground()
		for _, p := range pairs {
			ratio := Contrast(p.Surface, term)
			if ratio < p.MinimumRatio {
				t.Errorf("[%s] %s: delta %.3f:1 below minimum %.2f:1 (surface=%#v terminal=%#v)", variant, p.Name, ratio, p.MinimumRatio, p.Surface, term)
			}
			if p.MaximumRatio > 0 && ratio > p.MaximumRatio {
				t.Errorf("[%s] %s: delta %.3f:1 above maximum %.2f:1 -- too far from the terminal background (a \"bright slab\" again) (surface=%#v terminal=%#v)", variant, p.Name, ratio, p.MaximumRatio, p.Surface, term)
			}
		}
	}
}

func TestContrastIsSymmetric(t *testing.T) {
	white := lipgloss.Color("#FFFFFF")
	black := lipgloss.Color("#000000")
	if got := Contrast(white, black); got < 20 || got > 21.1 {
		t.Fatalf("Contrast(white, black) = %.2f, want ~21", got)
	}
	if got := Contrast(black, white); got < 20 || got > 21.1 {
		t.Fatalf("Contrast(black, white) = %.2f, want ~21 (order-independent)", got)
	}
	if got := Contrast(white, white); got < 0.99 || got > 1.01 {
		t.Fatalf("Contrast(white, white) = %.2f, want 1", got)
	}
}

func TestPaintOverExported(t *testing.T) {
	out := PaintOver("plain\x1b[0mtext", lipgloss.Color("#112233"), lipgloss.Color("#EEEEEE"))
	if !strings.Contains(plain(out), "plaintext") {
		t.Fatalf("PaintOver dropped content: %q", out)
	}
	want := fillSGR(lipgloss.Color("#112233"), lipgloss.Color("#EEEEEE"))
	if strings.Count(out, want) < 2 {
		t.Fatalf("PaintOver did not reassert the fill after the embedded reset: %q", out)
	}
}

func TestContentMarginsCollapsesBelowThreshold(t *testing.T) {
	if got := ContentMargins(MarginCollapseRows - 1); got != 0 {
		t.Fatalf("ContentMargins(%d) = %d, want 0 (below the collapse threshold)", MarginCollapseRows-1, got)
	}
	if got := ContentMargins(MarginCollapseRows); got != MarginRows {
		t.Fatalf("ContentMargins(%d) = %d, want %d (at the threshold, not collapsed)", MarginCollapseRows, got, MarginRows)
	}
	if got := ContentMargins(MarginCollapseRows + 10); got != MarginRows {
		t.Fatalf("ContentMargins(%d) = %d, want %d (well above the threshold)", MarginCollapseRows+10, got, MarginRows)
	}
}

func TestRenderHintsSplitsAnOverWideSegmentOnWordBoundaries(t *testing.T) {
	// A single plain segment wider than the whole line must wrap onto
	// multiple lines, breaking only between words -- never mid-word with
	// an ellipsis (the "F6/Shift+→ work…" regression this fixes).
	out := RenderHints(12, nil, "F6/Shift+→ workspace switch")
	flat := plain(out)
	for _, want := range []string{"F6/Shift+→", "workspace", "switch"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("word %q lost or cut mid-word while wrapping an over-wide segment:\n%s", want, flat)
		}
	}
	if strings.Contains(flat, "…") {
		t.Fatalf("expected no mid-word ellipsis truncation once a segment wraps on word boundaries:\n%s", flat)
	}
}
