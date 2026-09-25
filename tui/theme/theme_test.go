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
	if got := InnerWidth(10); got != 10-borderColumns-2*CardPadding {
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
	if cols != 2 || rows != 2 {
		t.Fatalf("ComposerFrameSize() = (%d, %d), want (2, 2)", cols, rows)
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
