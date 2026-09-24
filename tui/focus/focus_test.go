package focus

import "testing"

func TestNewStartsOnInput(t *testing.T) {
	r := New()
	if r.Zone() != ZoneInput {
		t.Fatalf("Zone() = %v, want ZoneInput", r.Zone())
	}
	if r.Stop() != -1 {
		t.Fatalf("Stop() = %d, want -1", r.Stop())
	}
}

func TestShiftUpFromEmptyInputFocusesLatestStop(t *testing.T) {
	r := New()
	if moved := r.ShiftUp(3); !moved {
		t.Fatal("ShiftUp did not move")
	}
	if r.Zone() != ZoneTranscript || r.Stop() != 2 {
		t.Fatalf("zone=%v stop=%d, want transcript/2", r.Zone(), r.Stop())
	}
}

func TestShiftUpWithNoStopsDoesNothing(t *testing.T) {
	r := New()
	if moved := r.ShiftUp(0); moved {
		t.Fatal("ShiftUp moved with zero stops")
	}
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input", r.Zone())
	}
}

func TestShiftUpDownWalkStops(t *testing.T) {
	r := New()
	r.ShiftUp(3) // -> stop 2
	if !r.ShiftUp(3) || r.Stop() != 1 {
		t.Fatalf("stop = %d, want 1", r.Stop())
	}
	if !r.ShiftUp(3) || r.Stop() != 0 {
		t.Fatalf("stop = %d, want 0", r.Stop())
	}
	if moved := r.ShiftUp(3); moved {
		t.Fatal("ShiftUp moved past the first stop")
	}
	if r.Stop() != 0 {
		t.Fatalf("stop = %d, want 0 (unchanged)", r.Stop())
	}
}

func TestShiftDownPastLastReturnsToInput(t *testing.T) {
	r := New()
	r.FocusStop(2)
	if !r.ShiftDown(3) {
		t.Fatal("ShiftDown(3) from stop 2 (last of 3) did not move")
	}
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input", r.Zone())
	}
}

func TestShiftDownMovesForward(t *testing.T) {
	r := New()
	r.FocusStop(0)
	if !r.ShiftDown(3) || r.Stop() != 1 {
		t.Fatalf("stop = %d, want 1", r.Stop())
	}
}

func TestShiftDownFromInputIsNoop(t *testing.T) {
	r := New()
	if moved := r.ShiftDown(3); moved {
		t.Fatal("ShiftDown from input moved")
	}
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input", r.Zone())
	}
}

func TestShiftRightAndLeftRoundTripFromTranscript(t *testing.T) {
	r := New()
	r.FocusStop(1)
	if !r.ShiftRight() {
		t.Fatal("ShiftRight did not move")
	}
	if r.Zone() != ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", r.Zone())
	}
	if !r.ShiftLeft(3) {
		t.Fatal("ShiftLeft did not move")
	}
	if r.Zone() != ZoneTranscript || r.Stop() != 1 {
		t.Fatalf("zone=%v stop=%d, want transcript/1", r.Zone(), r.Stop())
	}
}

func TestShiftRightAndLeftRoundTripFromInput(t *testing.T) {
	r := New()
	r.ShiftRight()
	r.ShiftLeft(0)
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input", r.Zone())
	}
}

func TestShiftLeftClampsReturnStopToCurrentStops(t *testing.T) {
	r := New()
	r.FocusStop(4) // e.g. focus on stop 4 of what was a 5-stop transcript
	r.ShiftRight()
	// The transcript shrank to 2 stops (0,1) while the sidebar had focus.
	if !r.ShiftLeft(2) {
		t.Fatal("ShiftLeft did not move")
	}
	if r.Zone() != ZoneTranscript || r.Stop() != 1 {
		t.Fatalf("zone=%v stop=%d, want transcript/1 (clamped)", r.Zone(), r.Stop())
	}
}

func TestShiftLeftClampsToInputWhenNoStopsRemain(t *testing.T) {
	r := New()
	r.FocusStop(2)
	r.ShiftRight()
	if !r.ShiftLeft(0) {
		t.Fatal("ShiftLeft did not move")
	}
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input (no stops left)", r.Zone())
	}
}

func TestShiftRightTwiceIsNoop(t *testing.T) {
	r := New()
	r.FocusStop(0)
	r.ShiftRight()
	if moved := r.ShiftRight(); moved {
		t.Fatal("second ShiftRight moved")
	}
	if r.Zone() != ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", r.Zone())
	}
}

func TestShiftLeftFromNonSidebarIsNoop(t *testing.T) {
	r := New()
	if moved := r.ShiftLeft(0); moved {
		t.Fatal("ShiftLeft from input moved")
	}
}

func TestEscAlwaysReturnsToInput(t *testing.T) {
	r := New()
	r.FocusStop(2)
	r.ShiftRight()
	r.Esc()
	if r.Zone() != ZoneInput || r.Stop() != -1 {
		t.Fatalf("zone=%v stop=%d, want input/-1", r.Zone(), r.Stop())
	}
}

func TestFocusStopNegativeClampsToInput(t *testing.T) {
	r := New()
	r.FocusStop(2)
	r.FocusStop(-1)
	if r.Zone() != ZoneInput {
		t.Fatalf("zone = %v, want input", r.Zone())
	}
}

func TestFocusSidebarDirectlyIsNoopWhenAlreadyFocused(t *testing.T) {
	r := New()
	r.FocusStop(1)
	r.FocusSidebar()
	if r.Zone() != ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", r.Zone())
	}
	// Calling FocusSidebar again while already in the sidebar must not
	// clobber the remembered return zone/stop.
	r.FocusSidebar()
	if !r.ShiftLeft(3) {
		t.Fatal("ShiftLeft did not move")
	}
	if r.Zone() != ZoneTranscript || r.Stop() != 1 {
		t.Fatalf("zone=%v stop=%d, want transcript/1 (return state preserved)", r.Zone(), r.Stop())
	}
}

func TestShiftUpFromSidebarIsNoop(t *testing.T) {
	r := New()
	r.FocusStop(1)
	r.ShiftRight()
	if moved := r.ShiftUp(3); moved {
		t.Fatal("ShiftUp moved from sidebar")
	}
	if r.Zone() != ZoneSidebar {
		t.Fatalf("zone = %v, want sidebar", r.Zone())
	}
}

func TestZoneString(t *testing.T) {
	cases := map[Zone]string{ZoneInput: "input", ZoneTranscript: "transcript", ZoneSidebar: "sidebar"}
	for zone, want := range cases {
		if got := zone.String(); got != want {
			t.Errorf("Zone(%d).String() = %q, want %q", zone, got, want)
		}
	}
}
