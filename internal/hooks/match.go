package hooks

import (
	"slices"
	"strings"

	"github.com/noamsto/forged/forge"
)

// MatchEvent reports whether the event satisfies the rule's type, repo,
// from/to and conclusion filters. Author and branch need the change; see MatchChange.
func (r Rule) MatchEvent(ev forge.ChangeEvent) bool {
	if ev.Type != r.On || !slices.Contains(r.Repos, ev.Repo) {
		return false
	}
	if r.From != "" && ev.Old != r.From {
		return false
	}
	if r.To != "" && ev.New != r.To {
		return false
	}
	return r.Conclusion == "" || ev.New == r.Conclusion
}

// MatchChange applies the author and branch filters. An empty Author means the
// viewer; with no known viewer that matches nothing.
func (r Rule) MatchChange(ch forge.Change, viewer string) bool {
	switch r.Author {
	case "*":
	case "":
		if viewer == "" || !strings.EqualFold(ch.Author.Login, viewer) {
			return false
		}
	default:
		if !strings.EqualFold(ch.Author.Login, r.Author) {
			return false
		}
	}
	return r.Branch == "" || GlobMatch(r.Branch, ch.Head.Ref)
}

// GlobMatch matches s against pattern where '*' is any sequence (including '/'),
// '?' is one character and everything else is literal.
func GlobMatch(pattern, s string) bool {
	p, str := []rune(pattern), []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(str) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '?' || p[pi] == str[si]):
			pi++
			si++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
