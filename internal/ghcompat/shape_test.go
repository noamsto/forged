package ghcompat

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

var update = flag.Bool("update", false, "rewrite golden files from current output")

var allFields = []string{
	"number", "url", "title", "state", "isDraft", "headRefName", "headRefOid", "baseRefName",
	"mergeable", "mergeStateStatus", "reviewDecision", "statusCheckRollup", "autoMergeRequest",
	"updatedAt", "author",
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func newChange(repo, host string, number int) forge.Change {
	c := forge.Change{Repo: forge.RepoRef{Forge: "github", Host: host, Owner: "o", Name: repo}, Number: number}
	return c
}

// Equivalent of testdata/gh_reference_user.json.
func refUser() forge.Change {
	c := newChange("prdash", "github.com", 213)
	c.Repo.Owner = "noamsto"
	c.URL = "https://github.com/noamsto/prdash/pull/213"
	c.Title = "feat: launch prdash outside a repo with the all-repos board"
	c.Author = forge.Actor{Login: "noamsto", Name: "Noam Stolero", ID: "MDQ6VXNlcjE3OTMyMzI0"}
	c.State = forge.StateMerged
	c.Head.Ref = "feat/212-launch-prdash-outside-a-repo-with-the-al"
	c.Head.SHA = "d17de1aa2f343532a3d45c3d544750834aa28327"
	c.Base.Ref = "main"
	c.Mergeable = forge.MergeableUnknown
	c.MergeState = "unknown"
	c.ReviewDecision = forge.ReviewNone
	c.Checks = forge.Checks{Rollup: forge.RollupPass, Runs: []forge.CheckRun{{
		Kind: "check", Name: "gate", Workflow: "CI", Status: "completed", Conclusion: "success",
		URL:         "https://github.com/noamsto/prdash/actions/runs/45197482936/job/137771730030",
		StartedAt:   ts("2026-11-23T14:44:49Z"),
		CompletedAt: ts("2026-11-23T14:47:27Z"),
	}}}
	c.UpdatedAt = ts("2026-11-23T16:54:41Z")
	return c
}

// Equivalent of testdata/gh_reference_bot.json.
func refBot() forge.Change {
	c := newChange("cli", "github.com", 15145)
	c.Repo.Owner = "cli"
	c.URL = "https://github.com/cli/cli/pull/15145"
	c.Title = "chore(deps): Bump github.com/charmbracelet/x/ansi from 0.11.9 to 0.11.10"
	c.Author = forge.Actor{Login: "dependabot", IsBot: true}
	c.State = forge.StateMerged
	c.Head.Ref = "dependabot/go_modules/github.com/charmbracelet/x/ansi-0.11.10"
	c.Head.SHA = "b7d38765850266766f6f4662808dca6f84789afd"
	c.Base.Ref = "trunk"
	c.Mergeable = forge.MergeableUnknown
	c.MergeState = "unknown"
	c.ReviewDecision = forge.ReviewApproved
	c.Checks = forge.Checks{Rollup: forge.RollupPass, Runs: []forge.CheckRun{
		{
			Kind: "check", Name: "label-external", Workflow: "PR Triaging", Status: "completed", Conclusion: "skipped",
			URL:         "https://github.com/cli/cli/actions/runs/45190148370/job/137746127991",
			StartedAt:   ts("2026-11-23T14:03:51Z"),
			CompletedAt: ts("2026-11-23T14:03:42Z"),
		},
		{
			Kind: "check", Name: "label-external", Workflow: "PR Triaging", Status: "completed", Conclusion: "skipped",
			URL:         "https://github.com/cli/cli/actions/runs/45190148489/job/137746091417",
			StartedAt:   ts("2026-11-23T14:03:47Z"),
			CompletedAt: ts("2026-11-23T14:03:41Z"),
		},
	}}
	c.UpdatedAt = ts("2026-11-23T18:24:29Z")
	return c
}

// Equivalent of testdata/gh_reference_automerge.json.
func refAutoMerge() forge.Change {
	c := newChange("cli", "github.com", 15028)
	c.Repo.Owner = "cli"
	c.URL = "https://github.com/cli/cli/pull/15028"
	c.Title = "Bump Go to 1.27.4"
	c.Author = forge.Actor{Login: "github-actions", IsBot: true}
	c.State = forge.StateMerged
	c.Head.Ref = "bump-go-1.27.4"
	c.Head.SHA = "06e666b930ecd87e4146db42cef1addf8361dc76"
	c.Base.Ref = "trunk"
	c.Mergeable = forge.MergeableUnknown
	c.MergeState = "unknown"
	c.ReviewDecision = forge.ReviewApproved
	c.Checks = forge.Checks{Rollup: forge.RollupPass, Runs: []forge.CheckRun{
		{
			Kind: "check", Name: "CodeQL-Build (go, manual, ./.github/codeql/codeql-config.yml)", Workflow: "Code Scanning",
			Status: "completed", Conclusion: "success",
			URL:         "https://github.com/cli/cli/actions/runs/43036761514/job/130578227800",
			StartedAt:   ts("2026-11-11T14:17:01Z"),
			CompletedAt: ts("2026-11-11T14:20:04Z"),
		},
		{
			Kind: "check", Name: "lint", Workflow: "Lint", Status: "completed", Conclusion: "success",
			URL:         "https://github.com/cli/cli/actions/runs/43036761028/job/130578217153",
			StartedAt:   ts("2026-11-11T14:17:00Z"),
			CompletedAt: ts("2026-11-11T14:22:42Z"),
		},
	}}
	c.AutoMerge = &forge.AutoMerge{
		Method:         "merge",
		EnabledAt:      ts("2026-11-11T14:17:33Z"),
		EnabledBy:      forge.Actor{Login: "babakks", Name: "Babak K. Shandiz", ID: "MDQ6VXNlcjM2NzI4OTMx"},
		AuthorEmail:    new("babakks@github.com"),
		CommitHeadline: new("Merge pull request #15028 from cli/bump-go-1.27.4"),
		CommitBody:     new("Bump Go to 1.27.4"),
	}
	c.UpdatedAt = ts("2026-11-11T14:26:42Z")
	return c
}

// Open draft PR with conflicts, an in-progress CheckRun and two StatusContexts.
func statusContextChange() forge.Change {
	c := newChange("r", "github.com", 7)
	c.URL = "https://github.com/o/r/pull/7"
	c.Title = "wip: <b>html & stuff</b>"
	c.Author = forge.Actor{Login: "alice", ID: "U_1"}
	c.State = forge.StateOpen
	c.Draft = true
	c.Head.Ref = "wip/status"
	c.Head.SHA = "abc123"
	c.Base.Ref = "main"
	c.Mergeable = forge.MergeableConflict
	c.MergeState = "dirty"
	c.ReviewDecision = forge.ReviewChangesRequested
	c.Checks = forge.Checks{Rollup: forge.RollupFail, Runs: []forge.CheckRun{
		{
			Kind: "check", Name: "build", Workflow: "CI", Status: "in_progress",
			URL: "https://github.com/o/r/actions/runs/1", StartedAt: ts("2026-11-20T10:00:00Z"),
		},
		{
			Kind: "status", Name: "ci/legacy", Status: "completed", Conclusion: "failure",
			URL: "https://ci.example/1", StartedAt: ts("2026-11-20T10:01:00Z"),
		},
		{Kind: "status", Name: "deploy", Status: "pending"},
	}}
	c.UpdatedAt = ts("2026-11-20T10:05:00Z")
	return c
}

func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	var buf bytes.Buffer
	if err := Encode(&buf, v, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("%s mismatch\n got: %s\nwant: %s", name, buf.Bytes(), want)
	}
}

func TestGolden(t *testing.T) {
	t.Run("view_all_fields", func(t *testing.T) {
		checkGolden(t, "view_all_fields.golden", Shape(refUser(), allFields))
	})
	t.Run("list_all_fields", func(t *testing.T) {
		checkGolden(t, "list_all_fields.golden", []map[string]any{
			Shape(refBot(), allFields), Shape(refUser(), allFields),
		})
	})
	t.Run("view_status_context", func(t *testing.T) {
		checkGolden(t, "view_status_context.golden", Shape(statusContextChange(), allFields))
	})
	t.Run("view_bot_automerge", func(t *testing.T) {
		checkGolden(t, "view_bot_automerge.golden", Shape(refAutoMerge(), allFields))
	})
}

func TestShapeSubsetOnlyRequestedKeys(t *testing.T) {
	got := Shape(refUser(), []string{"number", "state"})
	want := map[string]any{"number": 213, "state": "MERGED"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestShapeEmptyRollupIsEmptyArray(t *testing.T) {
	c := refUser()
	c.Checks = forge.Checks{Rollup: forge.RollupNone}
	got := Shape(c, []string{"statusCheckRollup"})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"statusCheckRollup":[]}` {
		t.Errorf("got %s", b)
	}
}

func TestEncodeTTYIndents(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, map[string]any{"b": []int{1}, "a": "x<y"}, true); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": \"x<y\",\n  \"b\": [\n    1\n  ]\n}\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// normalize round-trips through JSON so Shape's Go types compare with decoded reference data.
func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// compareJSON checks key sets and value types recursively, and exact values for scalars.
func compareJSON(t *testing.T, path string, got, want any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: got %T, want object", path, got)
			return
		}
		for k := range w {
			if _, ok := g[k]; !ok {
				t.Errorf("%s: missing key %q", path, k)
			}
		}
		for k, gv := range g {
			wv, ok := w[k]
			if !ok {
				t.Errorf("%s: unexpected key %q", path, k)
				continue
			}
			compareJSON(t, path+"."+k, gv, wv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			t.Errorf("%s: got %v, want array of %d", path, got, len(w))
			return
		}
		for i := range w {
			compareJSON(t, path+"[]", g[i], w[i])
		}
	default:
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v, want %#v", path, got, want)
		}
	}
}

func TestShapeMatchesGhReference(t *testing.T) {
	cases := map[string]forge.Change{
		"gh_reference_user.json":      refUser(),
		"gh_reference_bot.json":       refBot(),
		"gh_reference_automerge.json": refAutoMerge(),
	}
	for file, change := range cases {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				t.Fatal(err)
			}
			var want any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			compareJSON(t, "$", normalize(t, Shape(change, allFields)), want)
		})
	}
}
