package chatshell

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type boundedMouseDialog struct {
	seen                         []tea.Msg
	local, bounded, closeOnClick bool
}

func (*boundedMouseDialog) View(int, int) string          { return "first text row\nselect this row" }
func (o *boundedMouseDialog) LocalMouseCoordinates() bool { return o.local }
func (o *boundedMouseDialog) BoundedMouseInput() bool     { return o.bounded }
func (o *boundedMouseDialog) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	o.seen = append(o.seen, msg)
	_, click := msg.(tea.MouseClickMsg)
	return o, func() tea.Msg { return "received" }, click && o.closeOnClick
}

func boundedMouseShell(t *testing.T, width, height int) (*Model, *boundedMouseDialog) {
	t.Helper()
	m := New(&fakeHandler{}, WithWorkspace(&fakeSidePanel{title: "Explorer"}))
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	o := &boundedMouseDialog{local: true, bounded: true}
	m.PushOverlay(o)
	return m, o
}

func boundedMouseTextPosition(t *testing.T, content, text string) (int, int) {
	t.Helper()
	for y, line := range strings.Split(ansi.Strip(content), "\n") {
		if at := strings.Index(line, text); at >= 0 {
			return ansi.StringWidth(line[:at]), y
		}
	}
	t.Fatalf("missing text %q: %s", text, ansi.Strip(content))
	return 0, 0
}

func TestWorkspaceBoundedMouseRejectsFrameAndOutside(t *testing.T) {
	for _, target := range []string{"left border", "left padding", "marker", "marker spacer", "right padding", "right border", "top border", "bottom border", "outside", "below"} {
		t.Run(target, func(t *testing.T) {
			m, o := boundedMouseShell(t, 100, 30)
			o.closeOnClick = true
			view := m.View().Content
			lines := strings.Split(ansi.Strip(view), "\n")
			x, y := boundedMouseTextPosition(t, view, "select this row")
			left := ansi.StringWidth(lines[y][:strings.Index(lines[y], "│")])
			right := ansi.StringWidth(lines[y][:strings.LastIndex(lines[y], "│")])
			switch target {
			case "left border":
				x = left
			case "left padding":
				x = left + 1
			case "marker":
				x = left + 2
			case "marker spacer":
				x = left + 3
			case "right padding":
				x = right - 1
			case "right border":
				x = right
			case "top border":
				_, y = boundedMouseTextPosition(t, view, "╭")
			case "bottom border":
				_, y = boundedMouseTextPosition(t, view, "╰")
			case "outside":
				x = -1
			case "below":
				y = 29
			}
			_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
			if len(o.seen) != 0 || cmd != nil || len(m.overlays) != 1 {
				t.Fatal("outside content click reached Update, returned command, or dismissed dialog")
			}
		})
	}
}

func TestWorkspaceBoundedMouseContentKeyboardAndResize(t *testing.T) {
	m, o := boundedMouseShell(t, 100, 30)
	for _, size := range []struct{ w, h int }{{100, 30}, {60, 18}, {20, 8}} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		view := m.View().Content
		for row, text := range []string{"first", "select"} {
			x, y := boundedMouseTextPosition(t, view, text)
			_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + 2, Y: y})
			if cmd == nil || len(o.seen) == 0 {
				t.Fatal("visible text did not receive click")
			}
			click := o.seen[len(o.seen)-1].(tea.MouseClickMsg)
			if click.X != 2 || click.Y != row {
				t.Fatalf("content coordinates = %#v", click)
			}
		}
	}
	before := len(o.seen)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(o.seen) != before+1 {
		t.Fatal("bounded dialog lost keyboard input")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 14})
	if cmd != nil || len(o.seen) != before+1 {
		t.Fatal("resize before View reused stale bounds")
	}
}

func TestWorkspaceBoundedMousePreRenderAndTinyViewport(t *testing.T) {
	for _, size := range []struct{ w, h int }{{100, 30}, {1, 30}, {0, 30}, {-1, 30}, {100, 0}, {100, 1}, {100, -1}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m, o := boundedMouseShell(t, size.w, size.h)
			_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 0})
			if cmd != nil || len(o.seen) != 0 {
				t.Fatal("unrendered dialog received mouse input")
			}
			if size.w == 100 && size.h == 30 {
				return
			}
			m.View()
			for _, point := range []struct{ x, y int }{{0, 0}, {4, 2}, {0, 14}} {
				_, cmd = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: point.x, Y: point.y})
				if cmd != nil || len(o.seen) != 0 {
					t.Fatal("clipped content received mouse input")
				}
			}
		})
	}
	m, o := boundedMouseShell(t, 100, 30)
	m.View()
	newTop := &boundedMouseDialog{local: true, bounded: true}
	m.PushOverlay(newTop)
	_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 14})
	if cmd != nil || len(newTop.seen) != 0 {
		t.Fatal("new overlay reused old bounds")
	}
	m.PopOverlay()
	m.View()
	m.PopOverlay()
	m.PushOverlay(o)
	_, cmd = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 14})
	if cmd != nil || len(o.seen) != 0 {
		t.Fatal("reopened overlay reused old bounds")
	}
}

func TestWorkspaceBoundedMouseOptInCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		workspace, local, bounded bool
	}{
		{"existing local", true, true, false}, {"global", true, false, true}, {"legacy layout", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, o := boundedMouseShell(t, 100, 30)
			m.workspaceLayout, o.local, o.bounded = tc.workspace, tc.local, tc.bounded
			m.View()
			o.closeOnClick = true
			_, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: -1, Y: -1})
			if cmd == nil || len(o.seen) != 1 || len(m.overlays) != 0 {
				t.Fatal("nonopted overlay lost outside input or dismissal")
			}
			click := o.seen[0].(tea.MouseClickMsg)
			if (!tc.workspace || !tc.local) && (click.X != -1 || click.Y != -1) {
				t.Fatal("global coordinates changed")
			}
		})
	}
}
