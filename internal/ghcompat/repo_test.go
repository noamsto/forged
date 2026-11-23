package ghcompat

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/noamsto/forged/forge"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRepo(t *testing.T, remotes map[string]string) string {
	t.Helper()
	isolateGit(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for name, url := range remotes {
		gitIn(t, dir, "remote", "add", name, url)
	}
	return dir
}

var wantOR = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

func TestResolveRepoExplicitSources(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir() // not a git repo: only non-git sources can succeed
	tests := []struct {
		name string
		req  Request
		env  map[string]string
		want forge.RepoRef
	}{
		{"-R wins over URL and GH_REPO", Request{Repo: "o/r", URL: "https://github.com/x/y/pull/3"}, map[string]string{"GH_REPO": "a/b"}, wantOR},
		{"URL repo", Request{URL: "https://github.com/o/r/pull/3", Number: 3}, nil, wantOR},
		{"URL wins over GH_REPO", Request{URL: "https://github.com/o/r/pull/3", Number: 3}, map[string]string{"GH_REPO": "a/b"}, wantOR},
		{"GH_REPO", Request{}, map[string]string{"GH_REPO": "o/r"}, wantOR},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, fb := ResolveRepo(context.Background(), tc.req, envOf(tc.env), dir)
			if fb != nil {
				t.Fatalf("fallback: %s", fb.Reason)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveRepoGitRemote(t *testing.T) {
	for _, url := range []string{"https://github.com/o/r.git", "git@github.com:o/r.git", "https://github.com/o/r"} {
		t.Run(url, func(t *testing.T) {
			dir := gitRepo(t, map[string]string{"origin": url})
			got, fb := ResolveRepo(context.Background(), Request{}, envOf(nil), dir)
			if fb != nil {
				t.Fatalf("fallback: %s", fb.Reason)
			}
			if got != wantOR {
				t.Errorf("got %+v, want %+v", got, wantOR)
			}
		})
	}
}

func TestResolveRepoIgnoresNonGitHubRemote(t *testing.T) {
	dir := gitRepo(t, map[string]string{
		"origin": "https://github.com/o/r.git",
		"mirror": "https://gitlab.com/o/r.git",
	})
	got, fb := ResolveRepo(context.Background(), Request{}, envOf(nil), dir)
	if fb != nil {
		t.Fatalf("fallback: %s", fb.Reason)
	}
	if got != wantOR {
		t.Errorf("got %+v, want %+v", got, wantOR)
	}
}

func TestResolveRepoFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"two GitHub remotes", func(t *testing.T) string {
			return gitRepo(t, map[string]string{
				"origin":   "https://github.com/o/r.git",
				"upstream": "https://github.com/up/r.git",
			})
		}},
		{"no remotes", func(t *testing.T) string { return gitRepo(t, nil) }},
		{"only a non-GitHub remote", func(t *testing.T) string {
			return gitRepo(t, map[string]string{"origin": "https://gitlab.com/o/r.git"})
		}},
		{"gh-resolved set", func(t *testing.T) string {
			dir := gitRepo(t, map[string]string{"origin": "https://github.com/o/r.git"})
			gitIn(t, dir, "config", "remote.origin.gh-resolved", "base")
			return dir
		}},
		{"not a git repo", func(t *testing.T) string {
			isolateGit(t)
			return t.TempDir()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			_, fb := ResolveRepo(context.Background(), Request{}, envOf(nil), dir)
			if fb == nil {
				t.Fatal("want fallback")
			}
			if fb.Reason == "" {
				t.Error("fallback without reason")
			}
		})
	}
}
