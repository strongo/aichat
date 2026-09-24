package session

import (
	"testing"
	"time"
)

func ref(id string) EntityRef {
	return EntityRef{Type: "happening", Keys: map[string]string{"id": id}}
}

func TestEntityRef_Same(t *testing.T) {
	a := EntityRef{Type: "happening", Keys: map[string]string{"spaceID": "s1", "id": "1"}, Title: "A"}
	b := EntityRef{Type: "happening", Keys: map[string]string{"spaceID": "s1", "id": "1"}, Title: "different title"}
	if !a.Same(b) {
		t.Error("expected Same to ignore Title and match on Type+Keys")
	}
	c := EntityRef{Type: "happening", Keys: map[string]string{"spaceID": "s1", "id": "2"}}
	if a.Same(c) {
		t.Error("different key value must not be Same")
	}
	d := EntityRef{Type: "task", Keys: map[string]string{"spaceID": "s1", "id": "1"}}
	if a.Same(d) {
		t.Error("different Type must not be Same")
	}
	e := EntityRef{Type: "happening", Keys: map[string]string{"id": "1"}}
	if a.Same(e) {
		t.Error("different key count must not be Same")
	}
}

func TestState_PinUnpin(t *testing.T) {
	var s State
	if !s.Pin(ref("1")) {
		t.Fatal("first pin should report change")
	}
	if s.Pin(ref("1")) {
		t.Fatal("pinning again should report no change")
	}
	if len(s.Sidebar) != 1 {
		t.Fatalf("sidebar = %v", s.Sidebar)
	}
	if !s.Unpin(ref("1")) {
		t.Fatal("unpin should report it was there")
	}
	if s.Unpin(ref("1")) {
		t.Fatal("unpin again should report false")
	}
	if len(s.Sidebar) != 0 {
		t.Fatalf("sidebar = %v, want empty", s.Sidebar)
	}
}

func TestState_Focus(t *testing.T) {
	var s State
	r := ref("1")
	s.Focus(&r)
	if s.Focused == nil || !s.Focused.Same(r) {
		t.Fatalf("Focused = %v", s.Focused)
	}
	s.Focus(nil)
	if s.Focused != nil {
		t.Fatalf("Focused = %v, want nil", s.Focused)
	}
}

func TestState_Candidates_Priority(t *testing.T) {
	focused := ref("focused")
	sel := ref("sel")
	lastShown := ref("last")
	prevTarget := ref("prev")
	sidebar1 := ref("sb1")
	sidebar2 := ref("sb2")

	s := State{
		Focused:   &focused,
		Selection: []EntityRef{sel},
		LastShown: []EntityRef{lastShown},
		Previous:  &Action{Target: &prevTarget},
		Sidebar:   []EntityRef{sidebar1, sidebar2},
	}
	got := s.Candidates()
	want := []EntityRef{focused, sel, lastShown, prevTarget, sidebar2, sidebar1}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Same(want[i]) {
			t.Errorf("candidate %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestState_Candidates_LastShownOnlyWhenSingle(t *testing.T) {
	s := State{LastShown: []EntityRef{ref("a"), ref("b")}}
	got := s.Candidates()
	if len(got) != 0 {
		t.Fatalf("Candidates() = %v, want empty when LastShown has >1 entries", got)
	}
}

func TestState_Candidates_Dedup(t *testing.T) {
	focused := ref("1")
	s := State{
		Focused:   &focused,
		Selection: []EntityRef{ref("1")},
		Sidebar:   []EntityRef{ref("1")},
	}
	got := s.Candidates()
	if len(got) != 1 {
		t.Fatalf("Candidates() = %v, want deduped to 1", got)
	}
}

func TestState_Candidates_Empty(t *testing.T) {
	var s State
	if got := s.Candidates(); len(got) != 0 {
		t.Fatalf("Candidates() = %v, want empty", got)
	}
}

func TestState_PreviousAt(t *testing.T) {
	now := time.Now()
	s := State{PreviousAt: now}
	if !s.PreviousAt.Equal(now) {
		t.Fatalf("PreviousAt = %v", s.PreviousAt)
	}
}
