package grid

import (
	"testing"

	"github.com/strongo/aichat/tui/theme"
)

// TestHighlightedRowMeetsContrastInEveryState covers the r10 coordinator
// review directly: the grid's highlighted-row text/background pair was
// never enumerated by theme's own contrast test (it lives entirely inside
// this package, in rowStyle), and the UNFOCUSED-grid highlighted row
// paired a role surface's own foreground with theme.MutedColor() as the
// background — safe in only ONE Dark variant, since MutedColor() itself
// flips light/dark but that foreground didn't flip to match (the r10
// regression: "dark olive/black background with dark text" in light
// mode). Checks rowStyle(highlighted=true, focused) for BOTH focus states
// and BOTH Dark variants against WCAG AA normal text (4.5:1).
func TestHighlightedRowMeetsContrastInEveryState(t *testing.T) {
	const bodyTextMinRatio = 4.5
	prevDark := theme.Dark
	t.Cleanup(func() { theme.SetDark(prevDark) })
	for _, dark := range []bool{true, false} {
		theme.SetDark(dark)
		for _, focused := range []bool{true, false} {
			s := rowStyle(true, focused)
			fg, bg := s.GetForeground(), s.GetBackground()
			if ratio := theme.Contrast(fg, bg); ratio < bodyTextMinRatio {
				t.Errorf("dark=%v focused=%v: highlighted row contrast %.2f:1 below minimum %.2f:1 (fg=%#v bg=%#v)", dark, focused, ratio, bodyTextMinRatio, fg, bg)
			}
		}
	}
}
