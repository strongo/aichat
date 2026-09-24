// Package ctxmgr selects which ai.ContextBlock values go into a ChatRequest,
// balancing two goals: keep the LLM within a token budget, and keep the
// provider-cached prefix (the leading static blocks a provider such as
// Anthropic prompt-caches) as stable as possible across turns so caching
// actually pays off.
//
// The policy is intentionally simple for the MVP:
//
//  1. Required scopes (from a decision.Decision) are always included; when
//     there was no decision at all, call SelectAll instead of Select -- it
//     includes every available static scope, since the main LLM path needs
//     the full cached context to classify and answer in one inference.
//  2. A static scope, once sent in this conversation, is RETAINED on every
//     later turn even if no longer required -- dropping it would shift the
//     cached prefix and lose the cache. Only budget pressure drops it.
//  3. Static blocks keep the order they were first sent in, so the prefix
//     stays byte-stable turn to turn.
//  4. Only when the estimated token count (len(text)/4, a coarse but
//     dependency-free estimate) exceeds Policy.BudgetTokens does the Manager
//     drop retained-but-not-required static scopes, FROM THE TAIL of the
//     stable order inward (never from the middle or front), so whatever
//     survives is still the stable, cacheable PREFIX of what came before --
//     dropping is never allowed to disturb it. A scope dropped this way is
//     also forgotten from "sent": it is not implicitly retained again next
//     turn just because it once appeared; it only comes back if required or
//     re-added in the normal course of scopes growing.
//  5. Dynamic blocks are included only for required scopes plus the
//     pinnedScopes the product passes (e.g. sidebar/focused entities the
//     user is actively looking at), since they are per-turn and not cached
//     anyway.
package ctxmgr

import (
	"sort"

	"github.com/strongo/aichat/ai"
)

// Policy configures a Manager.
type Policy struct {
	// BudgetTokens is the soft cap on estimated context tokens before the
	// Manager starts dropping retained-but-not-required static scopes
	// (default 12000).
	BudgetTokens int
}

func (p Policy) withDefaults() Policy {
	if p.BudgetTokens <= 0 {
		p.BudgetTokens = 12000
	}
	return p
}

// Report explains what Select/SelectAll did, for diagnostics (see ai/diag).
type Report struct {
	Required        []string
	Retained        []string
	Added           []string
	Dropped         []string
	EstimatedTokens int
	Compacted       bool
}

// Manager tracks, per conversation, which static scopes have already been
// sent (and so are candidates for provider-side caching) and their
// first-sent order.
type Manager struct {
	Policy Policy
	// sent records static scopes already sent, in first-sent order.
	sent []string
	// sentSet mirrors sent for O(1) membership checks.
	sentSet map[string]bool
}

// NewManager builds a Manager. A zero Policy uses defaults.
func NewManager(policy Policy) *Manager {
	return &Manager{Policy: policy.withDefaults(), sentSet: map[string]bool{}}
}

// Select picks the ai.ContextBlock values for the next request when a
// decision.Decision was made. required is the set of scopes THIS turn needs
// (from decision.Decision.RequiredScopes; an empty, non-nil slice is a valid
// "this decision needs no scopes", distinct from "no decision was made" --
// see SelectAll for that case). available is every context block the
// product can supply. pinnedScopes are scopes of entities the user is
// actively focused on or has pinned, whose dynamic data should be included
// even when not required.
func (m *Manager) Select(required []string, available []ai.ContextBlock, pinnedScopes []string) ([]ai.ContextBlock, Report) {
	return m.selectImpl(required, false, available, pinnedScopes)
}

// SelectAll picks context for a turn with NO decision.Decision at all (Jev
// off, or every decision.Provider abstained/errored): every available
// static scope is included, plus every available dynamic scope for
// required-equivalent purposes, since the main LLM must classify and answer
// the turn in a single inference and needs the full cached context to do
// it. pinnedScopes still works the same as in Select.
func (m *Manager) SelectAll(available []ai.ContextBlock, pinnedScopes []string) ([]ai.ContextBlock, Report) {
	return m.selectImpl(nil, true, available, pinnedScopes)
}

func (m *Manager) selectImpl(required []string, noDecision bool, available []ai.ContextBlock, pinnedScopes []string) ([]ai.ContextBlock, Report) {
	requiredSet := toSet(required)
	pinnedSet := toSet(pinnedScopes)

	byScope := map[string][]ai.ContextBlock{}
	var allStaticScopes []string
	seenScope := map[string]bool{}
	for _, b := range available {
		byScope[b.Scope] = append(byScope[b.Scope], b)
		if b.Kind == ai.ContextStatic && !seenScope[b.Scope] {
			seenScope[b.Scope] = true
			allStaticScopes = append(allStaticScopes, b.Scope)
		}
	}

	wantStaticScopes := map[string]bool{}
	if noDecision {
		for _, s := range allStaticScopes {
			wantStaticScopes[s] = true
		}
	} else {
		for s := range requiredSet {
			wantStaticScopes[s] = true
		}
		for _, s := range m.sent {
			wantStaticScopes[s] = true // rule 2: retain previously-sent statics
		}
	}

	// Rule 3: order by first-sent order, then newly added scopes in the
	// order they appear in `available`.
	var orderedScopes []string
	for _, s := range m.sent {
		if wantStaticScopes[s] {
			orderedScopes = append(orderedScopes, s)
		}
	}
	var added []string
	for _, s := range allStaticScopes {
		if wantStaticScopes[s] && !m.sentSet[s] {
			orderedScopes = append(orderedScopes, s)
			added = append(added, s)
		}
	}

	retained := diffSorted(orderedScopes, added)

	// Build the static block list and estimate tokens.
	staticBlocksByScope := map[string][]ai.ContextBlock{}
	textLen := func(scope string) int {
		n := 0
		for _, b := range byScope[scope] {
			if b.Kind == ai.ContextStatic {
				n += len(b.Text)
			}
		}
		return n
	}
	for _, s := range orderedScopes {
		for _, b := range byScope[s] {
			if b.Kind == ai.ContextStatic {
				staticBlocksByScope[s] = append(staticBlocksByScope[s], b)
			}
		}
	}

	total := 0
	for _, s := range orderedScopes {
		total += textLen(s)
	}

	var dropped []string
	compacted := false
	if estimateTokens(total) > m.Policy.BudgetTokens {
		// Rule 4: drop from the TAIL of orderedScopes inward -- never from
		// the middle or front -- so what remains is still a stable prefix
		// of what was sent before. A required scope is never dropped even
		// if it happens to be at the tail.
		dropSet := map[string]bool{}
		for i := len(orderedScopes) - 1; i >= 0 && estimateTokens(total) > m.Policy.BudgetTokens; i-- {
			s := orderedScopes[i]
			if requiredSet[s] {
				continue
			}
			total -= textLen(s)
			dropSet[s] = true
			dropped = append(dropped, s)
			compacted = true
		}
		if len(dropSet) > 0 {
			var kept []string
			for _, s := range orderedScopes {
				if !dropSet[s] {
					kept = append(kept, s)
				}
			}
			orderedScopes = kept
			var keptRetained []string
			for _, s := range retained {
				if !dropSet[s] {
					keptRetained = append(keptRetained, s)
				}
			}
			retained = keptRetained
			// A dropped scope is forgotten from "sent" too: it must not be
			// implicitly retained again next turn just because it appeared
			// once before budget pressure removed it.
			m.forget(dropSet)
		}
	}

	// Update conversation memory: everything in orderedScopes is now "sent".
	for _, s := range orderedScopes {
		if !m.sentSet[s] {
			m.sentSet[s] = true
			m.sent = append(m.sent, s)
		}
	}

	// Assemble the final block list: static blocks for orderedScopes, then
	// dynamic blocks for required+pinned scopes.
	var out []ai.ContextBlock
	for _, s := range orderedScopes {
		out = append(out, staticBlocksByScope[s]...)
	}
	dynWant := map[string]bool{}
	for s := range requiredSet {
		dynWant[s] = true
	}
	for s := range pinnedSet {
		dynWant[s] = true
	}
	if noDecision {
		for _, s := range allStaticScopes {
			dynWant[s] = true
		}
	}
	for _, b := range available {
		if b.Kind == ai.ContextDynamic && dynWant[b.Scope] {
			out = append(out, b)
		}
	}

	report := Report{
		Required:        sortedKeys(requiredSet),
		Retained:        retained,
		Added:           added,
		Dropped:         dropped,
		EstimatedTokens: estimateTokens(total),
		Compacted:       compacted,
	}
	return out, report
}

// forget removes scopes from m.sent/m.sentSet.
func (m *Manager) forget(scopes map[string]bool) {
	if len(scopes) == 0 {
		return
	}
	var kept []string
	for _, s := range m.sent {
		if scopes[s] {
			delete(m.sentSet, s)
			continue
		}
		kept = append(kept, s)
	}
	m.sent = kept
}

func estimateTokens(charLen int) int { return (charLen + 3) / 4 }

func toSet(vals []string) map[string]bool {
	if vals == nil {
		return map[string]bool{}
	}
	s := make(map[string]bool, len(vals))
	for _, v := range vals {
		s[v] = true
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diffSorted returns the elements of all not present in subtract, preserving
// all's order.
func diffSorted(all, subtract []string) []string {
	sub := toSet(subtract)
	var out []string
	for _, s := range all {
		if !sub[s] {
			out = append(out, s)
		}
	}
	return out
}
