// Package rules is a deterministic, table-driven decision.Provider. Products
// supply the Rules; this package runs them in order and does no NLP of its
// own -- no big regex engine, just normalised exact-phrase matching helpers.
package rules

import (
	"context"
	"strings"
	"unicode"

	"github.com/strongo/aichat/ai/decision"
	"github.com/strongo/aichat/ai/session"
)

// Rule is one deterministic rule: Match inspects the raw user text and
// session state and either returns a decision (ok=true) or abstains.
type Rule struct {
	Name  string
	Match func(text string, st session.State) (decision.Decision, bool)
}

// Provider runs Rules in order and returns the first match.
type Provider struct {
	name  string
	rules []Rule
}

// New builds a Provider. name identifies it in decision.Trace/Attempt.
func New(name string, rules ...Rule) *Provider {
	return &Provider{name: name, rules: rules}
}

// Name implements decision.Provider.
func (p *Provider) Name() string { return p.name }

// Decide implements decision.Provider: it runs each rule's Match in order
// against the normalised text and abstains if none match. Rules never block
// on ctx; this loop only checks it between rules so a caller's timeout still
// bounds a pathological rule set.
func (p *Provider) Decide(ctx context.Context, req decision.Request) (decision.Decision, bool, error) {
	text := Normalize(req.Text)
	for _, r := range p.rules {
		if ctx.Err() != nil {
			return decision.Decision{}, false, ctx.Err()
		}
		if d, ok := r.Match(text, req.State); ok {
			return d, true, nil
		}
	}
	return decision.Decision{}, false, nil
}

// Normalize lowercases text, collapses runs of whitespace to single spaces,
// trims leading/trailing punctuation and space, and strips a small set of
// trailing sentence punctuation (".", "!", "?") so rules can match on exact
// phrases regardless of casing or incidental punctuation.
func Normalize(text string) string {
	text = strings.ToLower(text)
	var b strings.Builder
	lastWasSpace := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			if !lastWasSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			lastWasSpace = true
			continue
		}
		lastWasSpace = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimRight(out, ".!? ")
	return out
}

// Phrases returns a matcher function that reports whether its input equals
// one of the given words exactly (words are matched as given -- callers
// normalise with Normalize first, typically via Rule.Match's text argument
// which Provider.Decide already normalises).
func Phrases(words ...string) func(string) bool {
	set := make(map[string]struct{}, len(words))
	for _, w := range words {
		set[Normalize(w)] = struct{}{}
	}
	return func(s string) bool {
		_, ok := set[s]
		return ok
	}
}
