package forge_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		in   string
		want forge.RepoRef
	}{
		{"o/r", forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}},
		{"github.com/o/r", forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}},
		{"ghe.corp/o/r", forge.RepoRef{Forge: "github", Host: "ghe.corp", Owner: "o", Name: "r"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := forge.ParseRepo(tt.in)
			if err != nil {
				t.Fatalf("ParseRepo(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRepo(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRepoRejects(t *testing.T) {
	for _, in := range []string{"", "o", "a/b/c/d", "o/"} {
		t.Run(in, func(t *testing.T) {
			if got, err := forge.ParseRepo(in); err == nil {
				t.Fatalf("ParseRepo(%q) = %+v, want error", in, got)
			}
		})
	}
}

func TestRepoRefString(t *testing.T) {
	r, err := forge.ParseRepo("o/r")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "github.com/o/r"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestKeyString(t *testing.T) {
	r, err := forge.ParseRepo("o/r")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		key  forge.Key
		want string
		list bool
	}{
		{"repo-open", forge.RepoOpen(r), "repo-open:github.com/o/r", true},
		{"change", forge.ChangeKey(r, 5), "change:github.com/o/r#5", false},
		{"change-by-branch", forge.ByBranch(r, "feat/x"), "change-by-branch:github.com/o/r@feat/x", true},
		{"query", forge.Query("github", "is:open author:@me", 30), "query:github:is:open author:@me#30", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			if got := tt.key.IsList(); got != tt.list {
				t.Fatalf("IsList() = %v, want %v", got, tt.list)
			}
		})
	}
}

func TestKeyComparable(t *testing.T) {
	r, err := forge.ParseRepo("o/r")
	if err != nil {
		t.Fatal(err)
	}
	m := map[forge.Key]int{forge.ChangeKey(r, 5): 1}
	if m[forge.ChangeKey(r, 5)] != 1 || m[forge.ChangeKey(r, 6)] != 0 {
		t.Fatal("Key does not behave as a map key")
	}
	if forge.Query("github", "q", 30) == forge.Query("github", "q", 50) {
		t.Fatal("limit must be part of the query key")
	}
}

func sampleChange(t *testing.T) forge.Change {
	t.Helper()
	r, err := forge.ParseRepo("o/r")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := forge.Change{
		Repo:           r,
		Number:         5,
		URL:            "https://github.com/o/r/pull/5",
		Title:          "Add thing",
		Author:         forge.Actor{Login: "octo", Name: "Octo Cat", ID: "U_1", IsBot: true},
		State:          forge.State("open"),
		Draft:          true,
		Mergeable:      forge.Mergeable("conflict"),
		MergeState:     "dirty",
		ReviewDecision: forge.ReviewDecision("changes_requested"),
		Checks: forge.Checks{
			Rollup:    forge.Rollup("fail"),
			Truncated: true,
			Runs: []forge.CheckRun{{
				Kind:        "check",
				Name:        "build",
				Workflow:    "ci",
				URL:         "https://example.test/run/1",
				Status:      "completed",
				Conclusion:  "failure",
				StartedAt:   at,
				CompletedAt: at.Add(time.Minute),
			}},
		},
		UpdatedAt: at,
	}
	c.Head.Ref, c.Head.SHA = "feat/x", "abc123"
	c.Base.Ref = "main"
	return c
}

func TestChangeJSONRoundTrip(t *testing.T) {
	want := sampleChange(t)
	headline := "headline"
	want.AutoMerge = &forge.AutoMerge{
		Method:         "squash",
		EnabledAt:      want.UpdatedAt,
		EnabledBy:      forge.Actor{Login: "octo"},
		CommitHeadline: &headline,
	}

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got forge.Change
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestChangeJSONShape(t *testing.T) {
	b, err := json.Marshal(sampleChange(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["autoMerge"]; ok {
		t.Fatalf("autoMerge must be omitted when nil: %s", b)
	}
	for k, want := range map[string]any{
		"state":          "open",
		"mergeable":      "conflict",
		"mergeState":     "dirty",
		"reviewDecision": "changes_requested",
		"updatedAt":      "2026-01-02T03:04:05Z",
		"number":         float64(5),
		"draft":          true,
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	head, _ := m["head"].(map[string]any)
	if head["ref"] != "feat/x" || head["sha"] != "abc123" {
		t.Errorf("head = %v", m["head"])
	}
	base, _ := m["base"].(map[string]any)
	if base["ref"] != "main" {
		t.Errorf("base = %v", m["base"])
	}
	checks, _ := m["checks"].(map[string]any)
	if checks["rollup"] != "fail" || checks["truncated"] != true {
		t.Errorf("checks = %v", m["checks"])
	}
	runs, _ := checks["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", checks["runs"])
	}
	if s, _ := runs[0].(map[string]any)["startedAt"].(string); !strings.HasSuffix(s, "Z") {
		t.Errorf("run startedAt not RFC3339: %v", runs[0])
	}
}
