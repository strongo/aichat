package ctxmgr

import (
	"strings"
	"testing"

	"github.com/strongo/aichat/ai"
)

func block(scope string, kind ai.ContextKind, name, text string) ai.ContextBlock {
	return ai.ContextBlock{Scope: scope, Kind: kind, Name: name, Text: text}
}

func TestSelect_RequiredAlwaysIncluded(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill text"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill text"),
	}
	got, report := m.Select([]string{"calendar"}, available, nil)
	if len(got) != 1 || got[0].Scope != "calendar" {
		t.Fatalf("got = %+v", got)
	}
	if len(report.Required) != 1 || report.Required[0] != "calendar" {
		t.Fatalf("report.Required = %v", report.Required)
	}
}

func TestSelect_NoDecisionIncludesAllStatic(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
	}
	got, _ := m.Select(nil, available, nil)
	if len(got) != 2 {
		t.Fatalf("got = %+v, want both static scopes when required is nil", got)
	}
}

func TestSelect_RetainsPreviouslySentEvenIfNotRequired(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
	}
	// Turn 1: calendar required, sent.
	got1, _ := m.Select([]string{"calendar"}, available, nil)
	if len(got1) != 1 {
		t.Fatalf("got1 = %+v", got1)
	}
	// Turn 2: tasks required. calendar should be RETAINED even though not
	// required, so the cached prefix doesn't shrink/shift.
	got2, report2 := m.Select([]string{"tasks"}, available, nil)
	scopes := scopesOf(got2)
	if !contains(scopes, "calendar") || !contains(scopes, "tasks") {
		t.Fatalf("got2 scopes = %v, want both calendar (retained) and tasks (required)", scopes)
	}
	if !contains(report2.Retained, "calendar") {
		t.Errorf("report2.Retained = %v, want calendar", report2.Retained)
	}
}

func TestSelect_StaticOrderStable(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
		block("contacts", ai.ContextStatic, "skill", "contacts skill"),
	}
	got1, _ := m.Select([]string{"tasks"}, available, nil)
	if scopesOf(got1)[0] != "tasks" {
		t.Fatalf("got1 = %v", scopesOf(got1))
	}
	// Now require calendar too: calendar was not sent before, so it's
	// appended after tasks (first-sent order), not inserted before it.
	got2, _ := m.Select([]string{"calendar", "tasks"}, available, nil)
	scopes := scopesOf(got2)
	iTasks := indexOf(scopes, "tasks")
	iCalendar := indexOf(scopes, "calendar")
	if iTasks < 0 || iCalendar < 0 || iTasks > iCalendar {
		t.Fatalf("scopes = %v, want tasks (first-sent) before calendar (newly added)", scopes)
	}
}

func TestSelect_CompactsWhenOverBudget(t *testing.T) {
	m := NewManager(Policy{BudgetTokens: 5}) // tiny budget forces compaction
	available := []ai.ContextBlock{
		block("big", ai.ContextStatic, "skill", strings.Repeat("x", 400)),
		block("small", ai.ContextStatic, "skill", "y"),
	}
	// Turn 1: both required and sent (no compaction needed yet since both required).
	_, _ = m.Select([]string{"big", "small"}, available, nil)
	// Turn 2: only "small" required; "big" is retained-but-not-required and
	// large, so over budget it should be the one dropped.
	got, report := m.Select([]string{"small"}, available, nil)
	scopes := scopesOf(got)
	if contains(scopes, "big") {
		t.Errorf("scopes = %v, want big dropped once over budget", scopes)
	}
	if !contains(scopes, "small") {
		t.Errorf("scopes = %v, want small (required) kept", scopes)
	}
	if !report.Compacted {
		t.Error("report.Compacted = false, want true")
	}
	if !contains(report.Dropped, "big") {
		t.Errorf("report.Dropped = %v, want big", report.Dropped)
	}
}

func TestSelect_NeverDropsRequiredEvenOverBudget(t *testing.T) {
	m := NewManager(Policy{BudgetTokens: 1})
	available := []ai.ContextBlock{
		block("huge", ai.ContextStatic, "skill", strings.Repeat("x", 4000)),
	}
	got, _ := m.Select([]string{"huge"}, available, nil)
	if len(got) != 1 {
		t.Fatalf("got = %+v, required scope must never be dropped", got)
	}
}

func TestSelect_DynamicOnlyForRequiredOrPinned(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("calendar", ai.ContextDynamic, "today", "today's happenings"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
		block("tasks", ai.ContextDynamic, "today", "today's tasks"),
	}
	got, _ := m.Select([]string{"calendar"}, available, []string{"tasks"})
	var dynScopes []string
	for _, b := range got {
		if b.Kind == ai.ContextDynamic {
			dynScopes = append(dynScopes, b.Scope)
		}
	}
	if !contains(dynScopes, "calendar") || !contains(dynScopes, "tasks") {
		t.Fatalf("dynScopes = %v, want calendar (required) and tasks (pinned)", dynScopes)
	}
}

func TestSelect_DynamicExcludedWhenNeitherRequiredNorPinned(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
		block("tasks", ai.ContextDynamic, "today", "today's tasks"),
	}
	// tasks retained via a prior turn but not required/pinned now.
	_, _ = m.Select([]string{"tasks"}, available, nil)
	got, _ := m.Select([]string{"calendar"}, available, nil)
	for _, b := range got {
		if b.Kind == ai.ContextDynamic && b.Scope == "tasks" {
			t.Fatalf("tasks dynamic block included though not required or pinned: %+v", got)
		}
	}
}

func TestSelect_EstimatedTokensReported(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{block("calendar", ai.ContextStatic, "skill", "12345678")} // 8 chars -> 2 tokens
	_, report := m.Select([]string{"calendar"}, available, nil)
	if report.EstimatedTokens != 2 {
		t.Errorf("EstimatedTokens = %d, want 2", report.EstimatedTokens)
	}
}

func scopesOf(blocks []ai.ContextBlock) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range blocks {
		if b.Kind == ai.ContextStatic && !seen[b.Scope] {
			seen[b.Scope] = true
			out = append(out, b.Scope)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	return indexOf(ss, s) >= 0
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}
