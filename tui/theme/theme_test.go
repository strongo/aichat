package theme

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
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

// withHalfBlockEdges forces HalfBlockEdgesActive() to report trueColor
// (true) or a downsampled profile (false), independent of the real
// environment `go test` runs in (which, having no COLORTERM set, reports
// half-block edges INACTIVE by default -- every existing pre-r9 test
// exercises the fallback path unless it opts into this).
func withHalfBlockEdges(t *testing.T, trueColor bool, fn func()) {
	t.Helper()
	prevEdges, prevDetect := HalfBlockEdges, detectColorProfile
	HalfBlockEdges = true
	detectColorProfile = func() colorprofile.Profile {
		if trueColor {
			return colorprofile.TrueColor
		}
		return colorprofile.ANSI256
	}
	t.Cleanup(func() { HalfBlockEdges, detectColorProfile = prevEdges, prevDetect })
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

// TestStatusLineHasNoBackgroundAndHintsInsetPadding covers StatusLine
// directly (chatshell's legacy plain-SetStatus default path is the only
// other caller, in a different package/test binary, so this package's
// own coverage needs its own call) — founder, r12, verbatim: "status
// line should have ... no background ... Should have horizontal
// padding."
func TestStatusLineHasNoBackgroundAndHintsInsetPadding(t *testing.T) {
	out := StatusLine(20, "hello")
	if containsBackgroundSGR(out) {
		t.Fatalf("StatusLine painted a background SGR: %q", out)
	}
	left, _ := StatusInset()
	if !strings.HasPrefix(ansi.Strip(out), strings.Repeat(" ", left)+"hello") {
		t.Fatalf("StatusLine = %q, want to start with %d blank columns then the content", out, left)
	}
	if w := lipgloss.Width(out); w != 20 {
		t.Fatalf("StatusLine width = %d, want 20", w)
	}
	// Overflowing content still truncates with an ellipsis (unlike
	// RenderHints' own already-wrapped lines -- see hintsWrappedLine's
	// own doc for why THAT path must not).
	overflow := StatusLine(10, "this is far too long to fit")
	if !strings.Contains(overflow, "…") {
		t.Fatalf("StatusLine overflow = %q, want an ellipsis", overflow)
	}
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
	// r12's HintsInset() padding narrows RenderHints' own packing budget
	// below the nominal width, so a plain multi-word segment like "session
	// summary line" may now itself wrap across lines (each individual WORD
	// is still checked, not the exact joined phrase -- wrapTokens is free
	// to place its words on different lines, just never lose or cut one).
	for _, want := range []string{"session", "summary", "line", "navigate", "send", "quit"} {
		if !strings.Contains(flat, want) {
			t.Errorf("wrapped hints dropped %q:\n%s", want, flat)
		}
	}
	left, right := HintsInset()
	for i, line := range lines {
		w := lipgloss.Width(line)
		// Every line is exactly 24 UNLESS it holds a single atomic token
		// (a hint's key/label pair, or a lone word) wider than the inset
		// packing budget on its own -- wrapTokens still gives it its own
		// line rather than cutting it (see wrapTokens' own doc), so that
		// one line is allowed to render WIDER than 24 instead.
		if w != 24 && w <= 24-left-right {
			t.Errorf("wrapped line %d width = %d, want 24 (or wider, only for an atomic over-wide token): %q", i, w, line)
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

// TestComposerTextColumnAndChipLeadingFillAgreeWithComposerFrameSize locks
// in the r11 chip-alignment constants directly against composerBarWidth/
// composerPaddingCols (via their one existing public witness,
// ComposerFrameSize) rather than repeating the literal 1/2 values, so a
// future change to either constant can't silently desync ComposerTextColumn/
// ComposerChipLeadingFill from what the composer itself actually renders.
func TestComposerTextColumnAndChipLeadingFillAgreeWithComposerFrameSize(t *testing.T) {
	cols, _ := ComposerFrameSize() // barWidth + 2*paddingCols
	barWidth := 1                  // composerBarWidth's only other public witness (surfaceFill's marker column) is always 1 column.
	paddingCols := (cols - barWidth) / 2
	if got := ComposerTextColumn(); got != barWidth+paddingCols {
		t.Fatalf("ComposerTextColumn() = %d, want %d (marker + left padding)", got, barWidth+paddingCols)
	}
	if got := ComposerChipLeadingFill(); got != paddingCols-1 {
		t.Fatalf("ComposerChipLeadingFill() = %d, want %d (left padding - 1, since a chip's own leading space covers the last column)", got, paddingCols-1)
	}
}

// TestComposerChipMarkerMatchesTopEdgeMarker locks in that
// ComposerChipMarker renders the SAME "▖" top-edge marker cell
// ComposerFrame's own top edge draws (blank when unfocused, ▖ in
// FocusColor() when focused) -- see markerCell's own doc -- so a chip
// row substituting for the composer's top edge (chatshell's
// composerUsesChipsAsTopEdge) shows no visible discontinuity.
func TestComposerChipMarkerMatchesTopEdgeMarker(t *testing.T) {
	for _, focused := range []bool{false, true} {
		got := ComposerChipMarker(focused)
		want := markerCell("▖", composerBarWidth, focused)
		if got != want {
			t.Fatalf("focused=%v: ComposerChipMarker() = %q, want %q (same as the composer's own top-edge marker)", focused, got, want)
		}
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
	bg, fg := ComposerColors()
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
	unfocused := PanelFrame(30, 3, "", "panel content", false)
	focused := PanelFrame(30, 3, "", "panel content", true)
	if unfocused == focused {
		t.Fatal("focused panel frame identical to unfocused")
	}
	if !strings.Contains(plain(unfocused), "panel content") || !strings.Contains(plain(focused), "panel content") {
		t.Fatalf("frame missing content: unfocused=%q focused=%q", unfocused, focused)
	}
}

// TestPanelContentInsetMatchesCardTextInset covers the 2026-09-25
// coordinator finding: panel header/rows started flush against the
// panel's own surface first column ("Beacon", "● Project" touching the
// edge) while every Card's own text sits CardPaddingCols in from ITS
// surface edge. Panel content must now sit that same number of columns in
// from the panel's OWN surface origin (the column right after
// panelGapWidth's gap) as Card's text sits from ITS surface origin (the
// column right after cardBarWidth's marker) -- the same text-column inset,
// not just the same numeric constant.
func TestPanelContentInsetMatchesCardTextInset(t *testing.T) {
	const width = 30
	cardLines := strings.Split(plain(Card(RoleUser, "", "Q", width, false)), "\n")
	cardTextCol := -1
	for _, line := range cardLines {
		if idx := strings.IndexRune(line, 'Q'); idx >= 0 {
			cardTextCol = idx
			break
		}
	}
	if cardTextCol < 0 {
		t.Fatalf("card content %q missing marker rune", cardLines)
	}
	cardInset := cardTextCol - cardBarWidth

	panelLines := strings.Split(plain(PanelFrame(width, 3, "", "Q", false)), "\n")
	panelTextCol := -1
	for _, line := range panelLines {
		if idx := strings.IndexRune(line, 'Q'); idx >= 0 {
			panelTextCol = idx
			break
		}
	}
	if panelTextCol < 0 {
		t.Fatalf("panel content %q missing marker rune", panelLines)
	}
	panelInset := panelTextCol - panelGapWidth

	if panelInset != cardInset {
		t.Fatalf("panel content inset from its own surface origin = %d, card text inset from its own surface origin = %d, want equal", panelInset, cardInset)
	}
	if panelInset != CardPaddingCols {
		t.Fatalf("panel content inset = %d, want CardPaddingCols (%d)", panelInset, CardPaddingCols)
	}
}

// TestPanelFrameSize covers the r12 redesign (founder, verbatim, seeing
// the rendered result in Warp: "side panel should be full height card"):
// rows is now a CONSTANT 2 (the same vPad-vs-edges equalisation
// ComposerFrameSize relies on), not 0 -- a caller's row budget must
// reserve PanelFrame's own top/bottom overhead the same way it already
// does for Card/ComposerFrame. cols is panelGapWidth's one divider column
// plus panelPaddingCols on BOTH sides of the content (2026-09-25 fix,
// same round as the outer-margin fix: cols used to claim 2 while
// PanelFrame only ever drew 1 gap column and no content padding at all --
// this now reflects what PanelFrame actually reserves, gap AND inset).
func TestPanelFrameSize(t *testing.T) {
	cols, rows := PanelFrameSize()
	if want := panelGapWidth + 2*panelPaddingCols; cols != want || rows != 2 {
		t.Fatalf("PanelFrameSize() = (%d, %d), want (%d, 2)", cols, rows, want)
	}
}

// TestPanelFrameIsAFullHeightSurfaceWithAFocusMarker supersedes the r10
// "plain gap, no background" test: r12 (founder, verbatim: "side panel
// should be full height card") makes PanelFrame a genuine filled surface
// (theme.SurfaceColors(), like Card), not a bare gap -- so every content
// row (AND any row past what content actually supplied, up to height)
// now DOES carry the surface's own background SGR, deliberately the
// opposite of the r10 "no background SGR anywhere" assertion. The ONE
// gap column before the surface (and, unfocused, the surface's own
// marker column too, since it renders blank) stays plain -- confirmed by
// checking the reserved marker COLUMN specifically, not the whole line.
// TestPanelFrameIsAFullHeightSurfaceWithATintedFocus covers the r12 full-
// height-card redesign AND its r13 supersession (founder, verbatim:
// "Side panel should NOT have left accent border treatment. Can we just
// slightly change background?"): NO marker glyph ("▌"/"▖"/"▘") anywhere
// in a focused panel any more -- focus is PanelFocusColors() vs
// PanelColors() instead (see TestPanelFocusSurfaceDeltaIsSubtle for the
// delta itself).
func TestPanelFrameIsAFullHeightSurfaceWithATintedFocus(t *testing.T) {
	const height = 4 // taller than the 1 line of content supplied below.
	unfocused := PanelFrame(30, height, "", "row one", false)
	focused := PanelFrame(30, height, "", "row one", true)

	unfocusedLines := strings.Split(unfocused, "\n")
	focusedLines := strings.Split(focused, "\n")
	_, rows := PanelFrameSize()
	wantLines := height + rows
	if len(unfocusedLines) != wantLines || len(focusedLines) != wantLines {
		t.Fatalf("rendered %d/%d lines, want %d (height=%d + PanelFrameSize rows=%d)", len(unfocusedLines), len(focusedLines), wantLines, height, rows)
	}

	// Only the MIDDLE `height` rows are checked here -- the top/bottom
	// overhead rows (PanelFrameSize's own rows=2) carry no background AT
	// ALL in half-block mode by design (see HalfBlockEdge's own doc: the
	// glyph's fg IS the surface colour; there is no separate bg to set),
	// so this assertion is specifically about the CONTENT area, not the
	// edges.
	for i := 1; i < 1+height; i++ {
		if !containsBackgroundSGR(unfocusedLines[i]) {
			t.Fatalf("unfocused content row %d carries no background SGR at all (want the panel's own surface fill, even past its own content): %q", i, unfocusedLines[i])
		}
	}
	if !strings.Contains(plain(unfocused), "row one") {
		t.Fatalf("frame missing its own content: %q", unfocused)
	}

	for _, glyph := range []string{"▌", "▖", "▘"} {
		if strings.Contains(ansi.Strip(focused), glyph) {
			t.Fatalf("focused panel frame contains marker glyph %q, want none (r13: no left accent border): %q", glyph, focused)
		}
		if strings.Contains(ansi.Strip(unfocused), glyph) {
			t.Fatalf("unfocused panel frame contains marker glyph %q, want none: %q", glyph, unfocused)
		}
	}
	if unfocusedLines[1] == focusedLines[1] {
		t.Fatal("focused content row identical to unfocused -- expected the surface tint to change it")
	}
	if lipgloss.Width(unfocusedLines[0]) != lipgloss.Width(focusedLines[0]) {
		t.Fatal("focusing the panel changed its rendered width")
	}
}

// TestPanelFocusSurfaceDeltaIsSubtle covers the r13 redesign directly
// (founder, verbatim: "delta small but perceptible"): PanelFocusColors()'
// own background against PanelColors()' must land within
// [minPanelFocusDelta, maxPanelFocusDelta], and both pairs' own text must
// still clear bodyTextMinRatio -- checked in both theme.Dark variants and
// against every simulatedTerminalBackgrounds entry (a real terminal's
// reported background can be anything).
func TestPanelFocusSurfaceDeltaIsSubtle(t *testing.T) {
	check := func(t *testing.T) {
		t.Helper()
		unfocusedBG, unfocusedFG := PanelColors()
		focusedBG, focusedFG := PanelFocusColors()
		if delta := Contrast(focusedBG, unfocusedBG); delta < minPanelFocusDelta || delta > maxPanelFocusDelta {
			t.Errorf("panel focus/unfocus surface delta = %.3f, want within [%.2f, %.2f]", delta, minPanelFocusDelta, maxPanelFocusDelta)
		}
		if c := Contrast(unfocusedFG, unfocusedBG); c < bodyTextMinRatio {
			t.Errorf("unfocused panel text contrast = %.2f, want >= %.2f", c, bodyTextMinRatio)
		}
		if c := Contrast(focusedFG, focusedBG); c < bodyTextMinRatio {
			t.Errorf("focused panel text contrast = %.2f, want >= %.2f", c, bodyTextMinRatio)
		}
	}
	for _, dark := range []bool{true, false} {
		withDark(t, dark, func() { check(t) })
	}
	for _, hex := range simulatedTerminalBackgrounds {
		withTerminalBackground(t, hex, func() { check(t) })
	}
}

// TestPanelFrameHeaderRendersBoldAsFirstRow covers PanelFrame's header
// parameter directly (chatshell itself never passes one today -- both
// SidePanel and the default sidebar render their own title as their own
// content's first line -- but the parameter is still part of PanelFrame's
// own public contract and must work standalone).
func TestPanelFrameHeaderRendersBoldAsFirstRow(t *testing.T) {
	out := PanelFrame(30, 3, "My Panel", "row one", false)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[1], "My Panel") {
		t.Fatalf("header row = %q, want it to contain %q", lines[1], "My Panel")
	}
	if !strings.Contains(lines[1], "\x1b[1") {
		t.Fatalf("header row not bold: %q", lines[1])
	}
	if !strings.Contains(lines[2], "row one") {
		t.Fatalf("content row = %q, want it to contain %q", lines[2], "row one")
	}
}

// TestPanelFrameDropsContentPastItsOwnHeight covers the height-overflow
// branch: content taller than the height budget is silently clipped at
// height (a caller's own row budget, e.g. chatshell's panelInnerHeight,
// is the authority on how tall the panel's content area is — PanelFrame
// itself must never render MORE rows than it was asked for, matching
// Card/ComposerFrame's own "never taller than requested" contract).
func TestPanelFrameDropsContentPastItsOwnHeight(t *testing.T) {
	out := PanelFrame(30, 2, "", "row one\nrow two\nrow three", false)
	lines := strings.Split(out, "\n")
	_, rows := PanelFrameSize()
	if want := 2 + rows; len(lines) != want {
		t.Fatalf("rendered %d lines, want %d (height=2 + PanelFrameSize rows=%d)", len(lines), want, rows)
	}
	if strings.Contains(plain(out), "row three") {
		t.Fatalf("content past height=2 was not dropped: %q", out)
	}
}

// containsBackgroundSGR reports whether line contains ANY background SGR
// token, mirroring assertNoBackgroundSGR's own parsing (skipping a "38;2;
// r;g;b"/"38;5;n" foreground component's own numeric payload so it's
// never misread as a standalone background code) but inverted -- used by
// TestPanelFrameIsAFullHeightSurfaceWithAFocusMarker to assert the
// OPPOSITE of what the pre-r12 gap design guaranteed.
func containsBackgroundSGR(line string) bool {
	for _, seq := range regexp.MustCompile(`\x1b\[[0-9;]*m`).FindAllString(line, -1) {
		body := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
		parts := strings.Split(body, ";")
		for i := 0; i < len(parts); i++ {
			switch parts[i] {
			case "38", "39":
				if i+1 < len(parts) && parts[i+1] == "2" {
					i += 4
				} else if i+1 < len(parts) && parts[i+1] == "5" {
					i += 2
				}
			case "48", "40", "41", "42", "43", "44", "45", "46", "47",
				"100", "101", "102", "103", "104", "105", "106", "107":
				return true
			}
		}
	}
	return false
}

func TestSelectedRow(t *testing.T) {
	sel := SelectedRow("item", true, 20)
	unsel := SelectedRow("item", false, 20)
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

// TestSelectedRowFillsFullWidth covers the 2026-09-25 fix: a selected
// row's own highlight background must span its ENTIRE given width, not
// just "› "+text's own character count -- otherwise a short row's
// highlight reads as a narrow chip instead of a full-width selected bar,
// unlike every other selected/highlighted surface this package draws
// (grid's own rowStyle, Card's FocusSurfaceColors() fill).
func TestSelectedRowFillsFullWidth(t *testing.T) {
	const width = 24
	sel := SelectedRow("x", true, width)
	lines := strings.Split(sel, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected a single rendered line, got %d: %q", len(lines), sel)
	}
	if got := lipgloss.Width(lines[0]); got != width {
		t.Fatalf("SelectedRow(width=%d) rendered width = %d, want %d", width, got, width)
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

func TestHexRendersLowercaseRRGGBB(t *testing.T) {
	if got := Hex(lipgloss.Color("#1A5FC7")); got != "#1a5fc7" {
		t.Fatalf("Hex(#1A5FC7) = %q, want %q", got, "#1a5fc7")
	}
	if got := Hex(lipgloss.Color("#000000")); got != "#000000" {
		t.Fatalf("Hex(#000000) = %q, want %q", got, "#000000")
	}
	if got := Hex(lipgloss.Color("#ffffff")); got != "#ffffff" {
		t.Fatalf("Hex(#ffffff) = %q, want %q", got, "#ffffff")
	}
}

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

func TestPadRightNoopWhenAlreadyWideEnough(t *testing.T) {
	if got := padRight("already wide", 5); got != "already wide" {
		t.Fatalf("padRight with content already >= width = %q, want it unchanged", got)
	}
	if got := padRight("hi", 5); got != "hi   " {
		t.Fatalf("padRight(\"hi\", 5) = %q, want %q", got, "hi   ")
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

// TestCardHalfBlockEdgesUseSurfaceAndTerminalColours covers the founder's
// half-block-edge idea directly: with HalfBlockEdgesActive() true, a
// Card's first and last rendered lines must be "▄"/"▀" glyphs coloured
// foreground=the card's own surface fill, background=TerminalBackground()
// -- not a full blank padding row.
// TestSurfaceRowsAllSpanTheSameColumns covers the r13 founder report,
// verbatim: "It looks like ask anything line has narrower background
// then top and bottom lines" -- confirmed on both the composer and
// message cards, the content rows' surface fill ending ~4 columns (2x
// CardPaddingCols/composerPaddingCols) before the ▄/▀ edge rows. Root
// cause: surfaceFill's own lipgloss Style used Width(width-barWidth-
// 2*paddingCols) THEN ALSO applied Padding(vPad, paddingCols) inside that
// already-narrowed box -- lipgloss's Width counts padding INSIDE the
// width it's given (verified directly against the library), so the
// padding ate a second time into a box that had already excluded it,
// silently shrinking every content row 2*paddingCols short of the edge
// rows' own width-barWidth span. Checks EVERY row surfaceFill returns
// (content rows including their own right padding, AND the ▄/▀ edge
// rows when HalfBlockEdgesActive()) has the IDENTICAL rendered display
// width, for Card (single-line and multi-line body), ComposerFrame
// (empty, one line, multi-line), ComposerFrameNoTopEdge, and PanelFrame
// -- both with and without half-block edges active, since the padding
// double-subtraction bug applied to the FALLBACK (plain padding row)
// mode identically.
func TestSurfaceRowsAllSpanTheSameColumns(t *testing.T) {
	assertUniformWidth := func(t *testing.T, label, out string) {
		t.Helper()
		lines := strings.Split(out, "\n")
		want := lipgloss.Width(lines[0])
		for i, line := range lines {
			if w := lipgloss.Width(line); w != want {
				t.Errorf("%s: row %d width = %d, want %d (same as row 0) -- surface rows must all span the same columns:\n%s", label, i, w, want, out)
			}
		}
	}
	run := func(t *testing.T) {
		t.Helper()
		assertUniformWidth(t, "Card single-line", Card(RoleAssistant, "Assistant", "hello world", 40, false))
		assertUniformWidth(t, "Card multi-line", Card(RoleAssistant, "Assistant", "line one\nline two\nline three", 40, false))
		assertUniformWidth(t, "Card focused", Card(RoleAssistant, "Assistant", "hello world", 40, true))
		assertUniformWidth(t, "ComposerFrame empty", ComposerFrame(40, "", false))
		assertUniformWidth(t, "ComposerFrame one line", ComposerFrame(40, "Ask anything...", false))
		assertUniformWidth(t, "ComposerFrame multi-line", ComposerFrame(40, "line one\nline two", false))
		assertUniformWidth(t, "ComposerFrame focused", ComposerFrame(40, "Ask anything...", true))
		assertUniformWidth(t, "ComposerFrameNoTopEdge", ComposerFrameNoTopEdge(40, "Ask anything...", false))
		assertUniformWidth(t, "PanelFrame unfocused", PanelFrame(30, 4, "Panel", "row one\nrow two", false))
		assertUniformWidth(t, "PanelFrame focused", PanelFrame(30, 4, "Panel", "row one\nrow two", true))
	}
	t.Run("half-block edges active", func(t *testing.T) {
		withHalfBlockEdges(t, true, func() { run(t) })
	})
	t.Run("fallback mode", func(t *testing.T) {
		withHalfBlockEdges(t, false, func() { run(t) })
	})
}

func TestCardHalfBlockEdgesUseSurfaceAndTerminalColours(t *testing.T) {
	withHalfBlockEdges(t, true, func() {
		withDark(t, true, func() {
			out := Card(RoleAssistant, "Assistant", "hello world", 40, false)
			lines := strings.Split(out, "\n")
			if len(lines) < 3 {
				t.Fatalf("expected at least 3 lines (top edge, content, bottom edge): %q", out)
			}
			bg, _, _ := colorsFor(RoleAssistant)
			wantEdge := HalfBlockEdge(40-cardBarWidth, bg, true)
			if !strings.Contains(lines[0], "▄") {
				t.Fatalf("top line missing the half-block glyph: %q", lines[0])
			}
			if !strings.HasSuffix(lines[0], wantEdge) {
				t.Fatalf("top edge line = %q, want it to end with %q", lines[0], wantEdge)
			}
			last := lines[len(lines)-1]
			if !strings.Contains(last, "▀") {
				t.Fatalf("bottom line missing the half-block glyph: %q", last)
			}
			if strings.Contains(out, "hello world") == false {
				t.Fatalf("body content lost: %q", out)
			}
		})
	})
}

// TestCardFallsBackToFullPaddingWithoutTrueColor covers the opt-out/
// degradation path: HalfBlockEdges=false, or a non-TrueColor profile, must
// render the ORIGINAL full blank padding row (no "▄"/"▀" anywhere) --
// unchanged from this package's pre-half-block rendering.
func TestCardFallsBackToFullPaddingWithoutTrueColor(t *testing.T) {
	// Gate 1: colour profile isn't TrueColor.
	withHalfBlockEdges(t, false, func() {
		out := Card(RoleAssistant, "Assistant", "hello world", 40, false)
		if strings.ContainsAny(out, "▄▀") {
			t.Fatalf("expected no half-block glyphs when the colour profile isn't TrueColor: %q", out)
		}
	})
	// Gate 2: HalfBlockEdges=false, even on a TrueColor profile.
	withHalfBlockEdges(t, true, func() {
		prev := HalfBlockEdges
		SetHalfBlockEdges(false)
		t.Cleanup(func() { SetHalfBlockEdges(prev) })
		out := Card(RoleAssistant, "Assistant", "hello world", 40, false)
		if strings.ContainsAny(out, "▄▀") {
			t.Fatalf("HalfBlockEdges=false must disable half-block edges even on a TrueColor profile: %q", out)
		}
	})
}

// TestHalfBlockEdgesActiveRequiresBothSwitches covers the two independent
// gates: the package-level on/off switch and colour-profile detection.
func TestHalfBlockEdgesActiveRequiresBothSwitches(t *testing.T) {
	prevEdges, prevDetect := HalfBlockEdges, detectColorProfile
	t.Cleanup(func() { HalfBlockEdges, detectColorProfile = prevEdges, prevDetect })

	HalfBlockEdges = true
	detectColorProfile = func() colorprofile.Profile { return colorprofile.TrueColor }
	if !HalfBlockEdgesActive() {
		t.Fatal("expected active: HalfBlockEdges=true, TrueColor profile")
	}

	detectColorProfile = func() colorprofile.Profile { return colorprofile.ANSI256 }
	if HalfBlockEdgesActive() {
		t.Fatal("expected inactive: ANSI256 profile despite HalfBlockEdges=true")
	}

	detectColorProfile = func() colorprofile.Profile { return colorprofile.TrueColor }
	HalfBlockEdges = false
	if HalfBlockEdgesActive() {
		t.Fatal("expected inactive: HalfBlockEdges=false despite TrueColor profile")
	}
}

// TestComposerFrameNoTopEdgeOmitsOnlyTheTopEdge covers the composer's
// chips-in-edge seam: ComposerFrameNoTopEdge must drop the top "▄" row
// (content starts immediately) while keeping the bottom "▀" row, and must
// equal plain ComposerFrame content-for-content once both are stripped of
// their edge rows.
func TestComposerFrameNoTopEdgeOmitsOnlyTheTopEdge(t *testing.T) {
	withHalfBlockEdges(t, true, func() {
		full := ComposerFrame(30, "type here", false)
		noTop := ComposerFrameNoTopEdge(30, "type here", false)
		fullLines := strings.Split(full, "\n")
		noTopLines := strings.Split(noTop, "\n")
		if len(noTopLines) != len(fullLines)-1 {
			t.Fatalf("ComposerFrameNoTopEdge has %d lines, want %d (ComposerFrame's %d minus its top edge row)", len(noTopLines), len(fullLines)-1, len(fullLines))
		}
		if strings.Contains(noTopLines[0], "▄") {
			t.Fatalf("ComposerFrameNoTopEdge's first line still has a top edge glyph: %q", noTopLines[0])
		}
		// The bottom edge row (last line) must still be present and equal.
		if fullLines[len(fullLines)-1] != noTopLines[len(noTopLines)-1] {
			t.Fatalf("bottom edge row differs:\nfull:  %q\nnoTop: %q", fullLines[len(fullLines)-1], noTopLines[len(noTopLines)-1])
		}
	})
	// Fallback: identical to ComposerFrame (no edge concept to omit).
	withHalfBlockEdges(t, false, func() {
		full := ComposerFrame(30, "type here", false)
		noTop := ComposerFrameNoTopEdge(30, "type here", false)
		if full != noTop {
			t.Fatalf("fallback mode: ComposerFrameNoTopEdge should equal ComposerFrame, got:\nfull:  %q\nnoTop: %q", full, noTop)
		}
	})
}

// TestHalfBlockEdgeNonPositiveWidth covers the width<=0 guard directly --
// a caller (e.g. chatshell filling zero leftover columns after chips fill
// a whole row) gets "", not a panic or a negative-length repeat.
func TestHalfBlockEdgeNonPositiveWidth(t *testing.T) {
	if got := HalfBlockEdge(0, lipgloss.Color("#112233"), true); got != "" {
		t.Fatalf("HalfBlockEdge(0, ...) = %q, want empty", got)
	}
	if got := HalfBlockEdge(-3, lipgloss.Color("#112233"), false); got != "" {
		t.Fatalf("HalfBlockEdge(-3, ...) = %q, want empty", got)
	}
}

// withTerminalBackground simulates a tea.BackgroundColorMsg having
// arrived with hex (e.g. "#1e222b") — SetTerminalBackground plus restore.
func withTerminalBackground(t *testing.T, hex string, fn func()) {
	t.Helper()
	prevReal, prevDark := realTerminalBackground, Dark
	SetTerminalBackground(lipgloss.Color(hex))
	t.Cleanup(func() { realTerminalBackground, Dark = prevReal, prevDark })
	fn()
}

// TestSetTerminalBackgroundDerivesDarkFromLuminance covers
// SetTerminalBackground's own Dark-derivation and TerminalBackground()'s
// use of it once set.
func TestSetTerminalBackgroundDerivesDarkFromLuminance(t *testing.T) {
	withTerminalBackground(t, "#1e222b", func() {
		if !Dark {
			t.Fatal("expected Dark=true for a near-black background")
		}
		if got := TerminalBackground(); got != lipgloss.Color("#1e222b") {
			t.Fatalf("TerminalBackground() = %#v, want the real reported background", got)
		}
	})
	withTerminalBackground(t, "#fafafa", func() {
		if Dark {
			t.Fatal("expected Dark=false for a near-white background")
		}
	})
	// nil clears it, reverting to the guessed default.
	prev := realTerminalBackground
	SetTerminalBackground(nil)
	t.Cleanup(func() { realTerminalBackground = prev })
	if realTerminalBackground != nil {
		t.Fatal("SetTerminalBackground(nil) should clear realTerminalBackground")
	}
}

// simulatedTerminalBackgrounds are the three real-world backgrounds the
// founder's r10 review asked surfaces to be verified against: a common
// dark default, a common light default, and an intentionally HUED one
// (Solarized dark) that a fixed-hex-pair scheme would not have adapted to
// at all.
var simulatedTerminalBackgrounds = []string{"#1e222b", "#fafafa", "#002b36"}

// TestSurfacesAdaptToRealTerminalBackground covers the r10 fix directly:
// with a simulated tea.BackgroundColorMsg in effect, EVERY card role and
// the composer (both focus states) must (a) actually use that real
// background as their blend base — Contrast(surface, real) stays inside
// the same bounds SurfaceDeltaPairs/ContrastPairs already enforce — and
// (b) still meet bodyTextMinRatio for their own text. Run for a plain
// dark, a plain light, and a HUED (Solarized) background.
func TestSurfacesAdaptToRealTerminalBackground(t *testing.T) {
	for _, hex := range simulatedTerminalBackgrounds {
		withTerminalBackground(t, hex, func() {
			for _, p := range SurfaceDeltaPairs() {
				ratio := Contrast(p.Surface, TerminalBackground())
				if ratio < p.MinimumRatio {
					t.Errorf("[%s] %s: delta %.3f below minimum %.2f", hex, p.Name, ratio, p.MinimumRatio)
				}
				if p.MaximumRatio > 0 && ratio > p.MaximumRatio {
					t.Errorf("[%s] %s: delta %.3f above maximum %.2f", hex, p.Name, ratio, p.MaximumRatio)
				}
			}
			for _, p := range ContrastPairs() {
				if ratio := Contrast(p.FG, p.BG); ratio < p.MinimumRatio {
					t.Errorf("[%s] %s: contrast %.3f below minimum %.2f", hex, p.Name, ratio, p.MinimumRatio)
				}
			}
		})
	}
}

// TestHalfBlockEdgeRowsPaintNoBackground covers the r10 BLOCKING fix,
// verbatim founder instruction: "Edge rows and any 'terminal background'
// cells must NOT set a background at all (default bg, SGR 49) — only the
// fg = surface colour on the ▄/▀ glyphs." Renders a Card and a
// ComposerFrame with half-block edges active and asserts the FIRST and
// LAST rendered lines (the edge rows) contain no background-setting SGR
// component at all (no bare "48;" anywhere).
func TestHalfBlockEdgeRowsPaintNoBackground(t *testing.T) {
	withHalfBlockEdges(t, true, func() {
		card := Card(RoleAssistant, "Assistant", "hello", 40, false)
		cardLines := strings.Split(card, "\n")
		assertNoBackgroundSGR(t, "card top edge", cardLines[0])
		assertNoBackgroundSGR(t, "card bottom edge", cardLines[len(cardLines)-1])

		composer := ComposerFrame(30, "type here", true)
		composerLines := strings.Split(composer, "\n")
		assertNoBackgroundSGR(t, "composer top edge", composerLines[0])
		assertNoBackgroundSGR(t, "composer bottom edge", composerLines[len(composerLines)-1])
	})
}

// assertNoBackgroundSGR fails if line contains any background-setting SGR
// component ("48;..." 24-bit/256-colour, or a plain "4X"/"10X" standard
// background code).
func assertNoBackgroundSGR(t *testing.T, label, line string) {
	t.Helper()
	for _, seq := range regexp.MustCompile(`\x1b\[[0-9;]*m`).FindAllString(line, -1) {
		body := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
		parts := strings.Split(body, ";")
		for i := 0; i < len(parts); i++ {
			switch parts[i] {
			case "38", "39":
				// Foreground: "38;2;r;g;b" (24-bit) or "38;5;n" (256) --
				// skip the whole component so its own numeric payload
				// (which may coincidentally equal e.g. "43") is never
				// misread as a standalone background code below.
				if i+1 < len(parts) && parts[i+1] == "2" {
					i += 4
				} else if i+1 < len(parts) && parts[i+1] == "5" {
					i += 2
				}
			case "48":
				t.Fatalf("%s: unexpected background SGR %q in %q", label, seq, line)
			case "40", "41", "42", "43", "44", "45", "46", "47",
				"100", "101", "102", "103", "104", "105", "106", "107":
				t.Fatalf("%s: unexpected background SGR %q in %q", label, seq, line)
			}
		}
	}
}

// TestAccentBarSpansHalfBlockEdges covers the r10 coordinator correction:
// "Accent bar must span the whole surface including the half-height
// edges" — a FOCUSED card/composer's top/bottom edge rows must start with
// barWidth columns rendered in FocusColor() (not the surface colour),
// while an UNFOCUSED one's edge rows are uniform (the bar "blends in").
// TestFocusMarkerSpansHalfBlockEdges covers the r10 coordinator's
// SUPERSEDING correction (a narrow OUTSIDE marker, not an in-surface
// accent bar): a FOCUSED card's top edge row starts with "▖" in
// FocusColor(), its content rows with "▌" in FocusColor(), and its bottom
// edge row with "▘" in FocusColor() — none of those marker cells carry
// ANY background SGR (the terminal's own default shows through, since the
// marker sits OUTSIDE the surface). An UNFOCUSED card's marker column is
// blank (plain spaces) throughout, and the OUTER width is identical
// either way (reserving the column, not shifting layout).
func TestFocusMarkerSpansHalfBlockEdges(t *testing.T) {
	withHalfBlockEdges(t, true, func() {
		focused := Card(RoleAssistant, "Assistant", "hello world", 40, true)
		unfocused := Card(RoleAssistant, "Assistant", "hello world", 40, false)
		focusedLines := strings.Split(focused, "\n")
		unfocusedLines := strings.Split(unfocused, "\n")
		if len(focusedLines) != len(unfocusedLines) {
			t.Fatalf("focused/unfocused line counts differ: %d vs %d", len(focusedLines), len(unfocusedLines))
		}

		topMarker := markerCell("▖", cardBarWidth, true)
		if !strings.HasPrefix(focusedLines[0], topMarker) {
			t.Fatalf("focused card's top edge should start with the '▖' marker %q, got %q", topMarker, focusedLines[0])
		}
		assertNoBackgroundSGR(t, "focused top marker", topMarker)

		bottomMarker := markerCell("▘", cardBarWidth, true)
		last := focusedLines[len(focusedLines)-1]
		if !strings.HasPrefix(last, bottomMarker) {
			t.Fatalf("focused card's bottom edge should start with the '▘' marker %q, got %q", bottomMarker, last)
		}
		assertNoBackgroundSGR(t, "focused bottom marker", bottomMarker)

		contentMarker := markerCell("▌", cardBarWidth, true)
		// lines[1] is the header row -- a content row. Only the MARKER
		// cell itself (not the rest of the line, which legitimately
		// carries the card's own FocusSurfaceColors() background) must be
		// background-free.
		if !strings.HasPrefix(focusedLines[1], contentMarker) {
			t.Fatalf("focused card's content row should start with the '▌' marker %q, got %q", contentMarker, focusedLines[1])
		}
		assertNoBackgroundSGR(t, "focused content marker", contentMarker)

		if strings.HasPrefix(unfocusedLines[0], topMarker) || strings.Contains(unfocusedLines[0], "▖") {
			t.Fatalf("unfocused card's top edge should NOT show the focus marker: %q", unfocusedLines[0])
		}
		if lipgloss.Width(focusedLines[0]) != lipgloss.Width(unfocusedLines[0]) {
			t.Fatalf("focusing changed the top edge's rendered width: focused=%d unfocused=%d", lipgloss.Width(focusedLines[0]), lipgloss.Width(unfocusedLines[0]))
		}
	})
}

// TestFocusMarkerFallbackCoversFullPaddingRows covers the fallback case:
// HalfBlockEdges=false still marks every row of a focused surface
// (including the full blank padding rows) with "▌", per founder,
// verbatim: "Fallback HalfBlockEdges=false: marker '▌' on all rows of the
// surface including the full padding rows."
func TestFocusMarkerFallbackCoversFullPaddingRows(t *testing.T) {
	withHalfBlockEdges(t, false, func() {
		out := Card(RoleAssistant, "Assistant", "hello", 40, true)
		lines := strings.Split(out, "\n")
		if len(lines) < 3 {
			t.Fatalf("expected padding rows in fallback mode: %q", out)
		}
		for i, line := range lines {
			if !strings.Contains(line, "▌") {
				t.Fatalf("fallback line %d missing the '▌' marker: %q", i, line)
			}
		}
	})
}

// TestChipDistinctFromComposer covers the r10 coordinator correction:
// "chip surface a distinct step from the composer surface" — checked
// against BOTH the guessed default AND all three simulatedTerminalBackgrounds,
// same as the card-vs-terminal floor.
func TestChipDistinctFromComposer(t *testing.T) {
	check := func(t *testing.T, label string) {
		t.Helper()
		for _, p := range ChipDeltaPairs() {
			if ratio := Contrast(p.Surface, p.ComposerBG); ratio < p.MinimumRatio {
				t.Errorf("[%s] %s: delta %.3f below minimum %.2f", label, p.Name, ratio, p.MinimumRatio)
			}
		}
	}
	for _, dark := range []bool{true, false} {
		SetDark(dark)
		check(t, fmt.Sprintf("dark=%v (guessed default)", dark))
	}
	for _, hex := range simulatedTerminalBackgrounds {
		withTerminalBackground(t, hex, func() {
			check(t, hex)
		})
	}
}

// TestBorderColorBothBranches covers BorderColor directly -- kept as
// public API (a product may still want "the one border colour rule")
// even though no aichat component itself calls it anymore after r10's
// gap/marker redesigns.
func TestBorderColorBothBranches(t *testing.T) {
	if got := BorderColor(true); got != FocusColor() {
		t.Fatalf("BorderColor(true) = %#v, want FocusColor()", got)
	}
	if got := BorderColor(false); got != MutedColor() {
		t.Fatalf("BorderColor(false) = %#v, want MutedColor()", got)
	}
}

// TestContrastTextMatchesSurfaceText covers ContrastText directly (its
// own package's coverage doesn't see tui/grid's cross-package use of it).
func TestContrastTextMatchesSurfaceText(t *testing.T) {
	bg := lipgloss.Color("#333333")
	if got, want := ContrastText(bg), surfaceText(bg); got != want {
		t.Fatalf("ContrastText(%v) = %#v, want surfaceText's own %#v", bg, got, want)
	}
}
