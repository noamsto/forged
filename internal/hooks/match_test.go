package hooks

import (
	"testing"

	"github.com/noamsto/forged/forge"
)

func repo(t *testing.T, s string) forge.RepoRef {
	t.Helper()
	r, err := forge.ParseRepo(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMatchEvent(t *testing.T) {
	app := repo(t, "acme/app")
	other := repo(t, "acme/other")
	tests := []struct {
		name string
		rule Rule
		ev   forge.ChangeEvent
		want bool
	}{
		{"type and repo", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app}, true},
		{"wrong type", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged},
			forge.ChangeEvent{Type: forge.EventDraftChanged, Repo: app}, false},
		{"wrong repo", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: other}, false},
		{"second repo", Rule{Repos: []forge.RepoRef{other, app}, On: forge.EventStateChanged},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app}, true},
		{"from match", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged, From: "open"},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app, Old: "open"}, true},
		{"from mismatch", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged, From: "open"},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app, Old: "closed"}, false},
		{"to match", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged, To: "merged"},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app, New: "merged"}, true},
		{"to mismatch", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged, To: "merged"},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app, New: "closed"}, false},
		{"conclusion match", Rule{Repos: []forge.RepoRef{app}, On: forge.EventChecksCompleted, Conclusion: "fail"},
			forge.ChangeEvent{Type: forge.EventChecksCompleted, Repo: app, New: "fail"}, true},
		{"conclusion mismatch", Rule{Repos: []forge.RepoRef{app}, On: forge.EventChecksCompleted, Conclusion: "fail"},
			forge.ChangeEvent{Type: forge.EventChecksCompleted, Repo: app, New: "pass"}, false},
		{"empty filters match any", Rule{Repos: []forge.RepoRef{app}, On: forge.EventStateChanged},
			forge.ChangeEvent{Type: forge.EventStateChanged, Repo: app, Old: "x", New: "y"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.MatchEvent(tt.ev); got != tt.want {
				t.Fatalf("MatchEvent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchChange(t *testing.T) {
	mk := func(login, ref string) forge.Change {
		var ch forge.Change
		ch.Author.Login = login
		ch.Head.Ref = ref
		return ch
	}
	tests := []struct {
		name   string
		rule   Rule
		ch     forge.Change
		viewer string
		want   bool
	}{
		{"default author is viewer", Rule{}, mk("noams", "x"), "noams", true},
		{"default author other user", Rule{}, mk("bob", "x"), "noams", false},
		{"case-insensitive viewer", Rule{}, mk("NoamS", "x"), "noams", true},
		{"empty viewer fails closed", Rule{}, mk("noams", "x"), "", false},
		{"empty viewer, empty author login", Rule{}, mk("", "x"), "", false},
		{"explicit author", Rule{Author: "Bob"}, mk("bob", "x"), "noams", true},
		{"explicit author mismatch", Rule{Author: "bob"}, mk("alice", "x"), "noams", false},
		{"explicit author ignores empty viewer", Rule{Author: "bob"}, mk("bob", "x"), "", true},
		{"anyone", Rule{Author: "*"}, mk("alice", "x"), "noams", true},
		{"anyone with empty viewer", Rule{Author: "*"}, mk("alice", "x"), "", true},
		{"branch glob crosses slash", Rule{Author: "*", Branch: "feat/*"}, mk("a", "feat/a/b"), "", true},
		{"branch glob mismatch", Rule{Author: "*", Branch: "feat/*"}, mk("a", "fix/a"), "", false},
		{"branch question mark", Rule{Author: "*", Branch: "v?"}, mk("a", "v1"), "", true},
		{"branch question mark needs one char", Rule{Author: "*", Branch: "v?"}, mk("a", "v"), "", false},
		{"author ok but branch bad", Rule{Branch: "feat/*"}, mk("noams", "main"), "noams", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rule.MatchChange(tt.ch, tt.viewer); got != tt.want {
				t.Fatalf("MatchChange = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"", "a", false},
		{"*", "", true},
		{"*", "a/b/c", true},
		{"feat/*", "feat/a/b", true},
		{"feat/*", "feat/", true},
		{"feat/*", "feat", false},
		{"*-wip", "x/y-wip", true},
		{"*-wip", "x/y-wip2", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
		{"v?", "v1", true},
		{"v?", "v12", false},
		{"v?", "v", false},
		{"a.b", "aXb", false},
		{"a.b", "a.b", true},
		{"a[b]", "a[b]", true},
		{"**", "abc", true},
		{"?*", "", false},
		{"é?", "éx", true},
	}
	for _, tt := range tests {
		if got := GlobMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}
