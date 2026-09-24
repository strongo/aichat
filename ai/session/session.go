// Package session holds the product-neutral conversational working state that
// helps resolve requests such as "move it to Friday": the focused entity, the
// current selection, sidebar pins, and the pending and previous actions.
//
// It stores stable entity references, never rendered snapshots. Products
// define entity types and key names ("happening" + spaceID/happeningID in
// Sneat, "recordset" + ... in DataTug).
package session

import (
	"slices"
	"time"
)

// EntityRef is a stable reference to a product entity.
type EntityRef struct {
	Type string            `json:"type" yaml:"type"`
	Keys map[string]string `json:"keys" yaml:"keys"` // e.g. {"spaceID": "...", "happeningID": "..."}
	// Title is a display hint for prompts and the sidebar. It may be stale;
	// products re-resolve the entity by Keys before acting on it.
	Title string `json:"title,omitempty" yaml:"title,omitempty"`
}

// Same reports whether two refs point at the same entity (Title ignored).
func (r EntityRef) Same(o EntityRef) bool {
	if r.Type != o.Type || len(r.Keys) != len(o.Keys) {
		return false
	}
	for k, v := range r.Keys {
		if o.Keys[k] != v {
			return false
		}
	}
	return true
}

// Action is a semantic, product-defined operation (never a storage write).
type Action struct {
	Kind   string            `json:"kind"` // e.g. "calendar.reschedule_happening"
	Target *EntityRef        `json:"target,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
	// Summary is the human confirmation text ("Move Dentist to Fri 16:00?").
	Summary string `json:"summary,omitempty"`
	// Undo, when set, is the action that reverses this one.
	Undo *Action `json:"undo,omitempty"`
}

// State is the working context of one chat session.
type State struct {
	Focused   *EntityRef  `json:"focused,omitempty"`
	Selection []EntityRef `json:"selection,omitempty"`
	Sidebar   []EntityRef `json:"sidebar,omitempty"`
	// Pending is an action awaiting user confirmation.
	Pending *Action `json:"pending,omitempty"`
	// Previous is the last executed action (for "undo", "that").
	Previous   *Action   `json:"previous,omitempty"`
	PreviousAt time.Time `json:"previousAt,omitzero"`
	// LastShown are the entities of the most recent structured output, in
	// display order, so "the second one" can resolve.
	LastShown []EntityRef `json:"lastShown,omitempty"`
}

// Pin adds ref to the sidebar unless already present. It reports whether the
// sidebar changed.
func (s *State) Pin(ref EntityRef) bool {
	if slices.ContainsFunc(s.Sidebar, ref.Same) {
		return false
	}
	s.Sidebar = append(s.Sidebar, ref)
	return true
}

// Unpin removes ref from the sidebar and reports whether it was there.
func (s *State) Unpin(ref EntityRef) bool {
	n := len(s.Sidebar)
	s.Sidebar = slices.DeleteFunc(s.Sidebar, ref.Same)
	return len(s.Sidebar) != n
}

// Focus sets the focused entity (nil clears it).
func (s *State) Focus(ref *EntityRef) { s.Focused = ref }

// Candidates returns the entities a pronoun ("it", "that") most plausibly
// refers to, most likely first: focused, selection, the sole last-shown
// entity, the previous action's target, then sidebar pins (newest first).
// Duplicates are removed. Products filter by type.
func (s *State) Candidates() []EntityRef {
	var out []EntityRef
	add := func(r EntityRef) {
		if !slices.ContainsFunc(out, r.Same) {
			out = append(out, r)
		}
	}
	if s.Focused != nil {
		add(*s.Focused)
	}
	for _, r := range s.Selection {
		add(r)
	}
	if len(s.LastShown) == 1 {
		add(s.LastShown[0])
	}
	if s.Previous != nil && s.Previous.Target != nil {
		add(*s.Previous.Target)
	}
	for i := len(s.Sidebar) - 1; i >= 0; i-- {
		add(s.Sidebar[i])
	}
	return out
}
