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

func TestSelectAll_IncludesAllStatic(t *testing.T) {
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
		block("tasks", ai.ContextStatic, "skill", "tasks skill"),
	}
	got, report := m.SelectAll(available, nil)
	if len(got) != 2 {
		t.Fatalf("got = %+v, want both static scopes with no decision", got)
	}
	if len(report.Required) != 0 {
		t.Errorf("report.Required = %v, want empty (SelectAll has no decision)", report.Required)
	}
}

func TestSelect_EmptyNonNilRequiredMeansZeroScopes(t *testing.T) {
	// A decision that explicitly required nothing (RequiredScopes == []) is
	// NOT the same as no decision at all -- Select must not fall back to
	// "include everything" for it. That is SelectAll's job.
	m := NewManager(Policy{})
	available := []ai.ContextBlock{
		block("calendar", ai.ContextStatic, "skill", "calendar skill"),
	}
	got, _ := m.Select([]string{}, available, nil)
	if len(got) != 0 {
		t.Fatalf("got = %+v, want empty: an explicit empty required list must not pull in every static scope", got)
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

func TestSelect_DropsFromTailPreservingPrefix(t *testing.T) {
	m := NewManager(Policy{BudgetTokens: 1})
	available := []ai.ContextBlock{
		block("first", ai.ContextStatic, "skill", "aa"),
		block("second", ai.ContextStatic, "skill", "bb"),
		block("third", ai.ContextStatic, "skill", "cc"),
	}
	// Turn 1: all three required and sent, in this order.
	_, _ = m.Select([]string{"first", "second", "third"}, available, nil)
	// Turn 2: nothing required; budget forces dropping. The survivor(s) must
	// be a PREFIX of [first, second, third], i.e. "second" can never be
	// dropped while "third" survives.
	got, report := m.Select([]string{}, available, nil)
	scopes := scopesOf(got)
	for i := 1; i < len(scopes); i++ {
		if indexOf([]string{"first", "second", "third"}, scopes[i]) < indexOf([]string{"first", "second", "third"}, scopes[i-1]) {
			t.Fatalf("scopes = %v, not a stable prefix of [first second third]", scopes)
		}
	}
	if len(scopes) > 0 && scopes[0] != "first" {
		t.Fatalf("scopes = %v, want to start with 'first' (the earliest-sent) if anything survives", scopes)
	}
	if !report.Compacted {
		t.Fatal("expected compaction")
	}
}

func TestSelect_DroppedScopeForgottenFromSent(t *testing.T) {
	m := NewManager(Policy{BudgetTokens: 1})
	available := []ai.ContextBlock{
		block("big", ai.ContextStatic, "skill", strings.Repeat("x", 400)),
	}
	// Turn 1: required and sent.
	_, _ = m.Select([]string{"big"}, available, nil)
	// Turn 2: not required, budget still tiny -> dropped (nothing else to
	// keep it under budget; "big" is the only scope so this call keeps it
	// only if required, otherwise it's dropped since it's not required).
	_, report := m.Select([]string{}, available, nil)
	if !contains(report.Dropped, "big") {
		t.Fatalf("report.Dropped = %v, want big dropped", report.Dropped)
	}
	// Turn 3: still not required. If "big" had NOT been forgotten from
	// "sent", rule 2 (retain previously-sent statics) would pull it back in
	// even though nothing requires it -- it must stay gone.
	got, _ := m.Select([]string{}, available, nil)
	if len(got) != 0 {
		t.Fatalf("got = %+v, want empty: a budget-dropped scope must not be implicitly retained again", got)
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

func TestForget_EmptyIsNoop(t *testing.T) {
	// forget's early-return-on-empty is defensive: with only one call site
	// (guarded by `len(dropSet) > 0`) it can't be reached through the
	// exported API, so exercise the unexported func directly from within
	// the package to cover it without weakening the guard.
	m := NewManager(Policy{})
	m.sent = []string{"a", "b"}
	m.sentSet = map[string]bool{"a": true, "b": true}
	m.forget(map[string]bool{})
	if len(m.sent) != 2 || !m.sentSet["a"] || !m.sentSet["b"] {
		t.Fatalf("forget(empty) mutated state: sent=%v sentSet=%v", m.sent, m.sentSet)
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
