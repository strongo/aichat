// Package focus is a small, pure focus-ring state machine shared by every
// chatshell screen. It replaces the boolean flags DataTug's chat UI used
// (gridFocused, messageFocused, workspaceFocused, ...) with a single source
// of truth: which Zone holds focus and, when the zone is Transcript, which
// "stop" (an index into the transcript's ordered list of focusable entries,
// e.g. result grids and user messages) is focused.
//
// Semantics (identical to DataTug's chat UI, generalised):
//
//   - Shift+Up from an empty input focuses the latest (bottom-most)
//     focusable transcript stop.
//   - Shift+Up / Shift+Down move between transcript stops.
//   - Shift+Down past the last stop returns focus to the input.
//   - Shift+Right moves focus to the sidebar, remembering where focus was so
//     Shift+Left can return to it.
//   - Shift+Left, from the sidebar, returns focus to the remembered zone/stop.
//   - Esc always returns focus to the input.
//
// Ring holds no rendering state and knows nothing about tea.Msg; callers
// decide which key presses trigger which method (see tui/chatshell).
package focus

// Zone identifies which region of the chat screen holds focus.
type Zone int

const (
	// ZoneInput is the composer (textarea).
	ZoneInput Zone = iota
	// ZoneTranscript is one of the transcript's focusable stops (Stop()).
	ZoneTranscript
	// ZoneSidebar is the working-context sidebar.
	ZoneSidebar
)

func (z Zone) String() string {
	switch z {
	case ZoneTranscript:
		return "transcript"
	case ZoneSidebar:
		return "sidebar"
	default:
		return "input"
	}
}

// Ring is the focus-ring state machine. The zero value is ready to use and
// starts focused on the input, matching New().
type Ring struct {
	zone Zone
	stop int // meaningful only when zone == ZoneTranscript

	// returnZone/returnStop remember focus as it was immediately before a
	// ShiftRight moved it to the sidebar, so ShiftLeft can restore it.
	returnZone Zone
	returnStop int
}

// New returns a Ring focused on the input.
func New() *Ring {
	return &Ring{zone: ZoneInput, stop: -1, returnStop: -1}
}

// Zone reports which region currently holds focus.
func (r *Ring) Zone() Zone { return r.zone }

// Stop reports the focused transcript stop index. It is only meaningful
// when Zone() == ZoneTranscript; otherwise it returns -1.
func (r *Ring) Stop() int {
	if r.zone != ZoneTranscript {
		return -1
	}
	return r.stop
}

// FocusInput focuses the composer directly (equivalent to Esc).
func (r *Ring) FocusInput() {
	r.zone = ZoneInput
	r.stop = -1
}

// FocusStop focuses transcript stop index directly, e.g. after a click or an
// Enter-driven navigation elsewhere in the product. index must be a valid
// stop (the caller is expected to have checked it against the current stop
// count); an out-of-range index is clamped to input focus.
func (r *Ring) FocusStop(index int) {
	if index < 0 {
		r.FocusInput()
		return
	}
	r.zone = ZoneTranscript
	r.stop = index
}

// FocusSidebar moves focus to the sidebar, remembering the current zone/stop
// so ShiftLeft can return to it. A no-op when already in the sidebar.
func (r *Ring) FocusSidebar() {
	if r.zone == ZoneSidebar {
		return
	}
	r.returnZone, r.returnStop = r.zone, r.stop
	r.zone = ZoneSidebar
	r.stop = -1
}

// ShiftUp implements Shift+Up. stops is the current number of focusable
// transcript entries. It reports whether focus moved.
func (r *Ring) ShiftUp(stops int) bool {
	switch r.zone {
	case ZoneInput:
		if stops <= 0 {
			return false
		}
		r.zone = ZoneTranscript
		r.stop = stops - 1
		return true
	case ZoneTranscript:
		if r.stop <= 0 {
			return false
		}
		r.stop--
		return true
	default: // ZoneSidebar: Shift+Up has no meaning there
		return false
	}
}

// ShiftDown implements Shift+Down: moves toward later transcript stops, and
// past the last stop returns focus to the input. It reports whether focus
// moved.
func (r *Ring) ShiftDown(stops int) bool {
	switch r.zone {
	case ZoneTranscript:
		if r.stop+1 < stops {
			r.stop++
			return true
		}
		r.FocusInput()
		return true
	default:
		return false
	}
}

// ShiftRight implements Shift+Right: moves focus into the sidebar. Callers
// gate this on the sidebar being visible/split. It reports whether focus
// moved.
func (r *Ring) ShiftRight() bool {
	if r.zone == ZoneSidebar {
		return false
	}
	r.FocusSidebar()
	return true
}

// ShiftLeft implements Shift+Left: returns focus from the sidebar to where
// it was before ShiftRight. It reports whether focus moved.
func (r *Ring) ShiftLeft() bool {
	if r.zone != ZoneSidebar {
		return false
	}
	r.zone = r.returnZone
	r.stop = r.returnStop
	return true
}

// Esc always returns focus to the input.
func (r *Ring) Esc() { r.FocusInput() }
