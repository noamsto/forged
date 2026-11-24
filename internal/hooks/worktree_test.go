package hooks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

func wtLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wtIsolate keeps the git calls made by production code away from the
// developer's configuration.
func wtIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("HOME", t.TempDir())
}

func wtGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// wtRepo returns a fresh clone-like repo with one commit on main.
func wtRepo(t *testing.T) string {
	t.Helper()
	wtIsolate(t)
	dir := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	wtGit(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wtGit(t, dir, "add", "f.txt")
	wtGit(t, dir, "commit", "-m", "init")
	return dir
}

// wtAddWorktree adds a worktree on a new branch and returns its path.
func wtAddWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(repo), strings.ReplaceAll(branch, "/", "_"))
	wtGit(t, repo, "worktree", "add", "-b", branch, p)
	return p
}

func wtChange(branch, sha string, state forge.State) forge.Change {
	var ch forge.Change
	ch.State = state
	ch.Head.Ref = branch
	ch.Head.SHA = sha
	ch.Author.Login = "me"
	return ch
}

func wtRule(repo string) Rule {
	return Rule{Repos: []forge.RepoRef{{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}}, Checkout: repo}
}

func wtListed(t *testing.T, repo string) string {
	return wtGit(t, repo, "worktree", "list", "--porcelain")
}

func wtExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestWorktreeRemoveClean(t *testing.T) {
	repo := wtRepo(t)
	wt := wtAddWorktree(t, repo, "feat/x")
	sha := wtGit(t, wt, "rev-parse", "HEAD")
	if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange("feat/x", sha, forge.StateMerged), wtLogger()); err != nil {
		t.Fatal(err)
	}
	if wtExists(wt) {
		t.Error("worktree dir still exists")
	}
	if strings.Contains(wtListed(t, repo), "feat/x") {
		t.Error("worktree still listed")
	}
}

func TestWorktreeKept(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo, wt string) (sha string)
	}{
		{"modified tracked file", func(t *testing.T, _, wt string) string {
			if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return wtGit(t, wt, "rev-parse", "HEAD")
		}},
		{"untracked file", func(t *testing.T, _, wt string) string {
			if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return wtGit(t, wt, "rev-parse", "HEAD")
		}},
		{"untracked file in new dir", func(t *testing.T, _, wt string) string {
			if err := os.MkdirAll(filepath.Join(wt, "d"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wt, "d", "new.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return wtGit(t, wt, "rev-parse", "HEAD")
		}},
		{"extra local commit", func(t *testing.T, _, wt string) string {
			sha := wtGit(t, wt, "rev-parse", "HEAD")
			wtGit(t, wt, "commit", "--allow-empty", "-m", "local")
			return sha
		}},
		{"head sha differs from pr", func(t *testing.T, _, _ string) string {
			return strings.Repeat("0", 40)
		}},
		{"locked", func(t *testing.T, repo, wt string) string {
			wtGit(t, repo, "worktree", "lock", wt)
			return wtGit(t, wt, "rev-parse", "HEAD")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := wtRepo(t)
			wt := wtAddWorktree(t, repo, "feat/x")
			sha := tc.setup(t, repo, wt)
			if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange("feat/x", sha, forge.StateMerged), wtLogger()); err != nil {
				t.Fatal(err)
			}
			if !wtExists(wt) {
				t.Error("worktree was removed")
			}
		})
	}
}

func TestWorktreeMainNeverRemoved(t *testing.T) {
	repo := wtRepo(t)
	sha := wtGit(t, repo, "rev-parse", "HEAD")
	if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange("main", sha, forge.StateMerged), wtLogger()); err != nil {
		t.Fatal(err)
	}
	if !wtExists(repo) || !wtExists(filepath.Join(repo, ".git")) {
		t.Error("main worktree was removed")
	}
}

func TestWorktreeNotCheckedOut(t *testing.T) {
	repo := wtRepo(t)
	wt := wtAddWorktree(t, repo, "feat/x")
	sha := wtGit(t, wt, "rev-parse", "HEAD")
	if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange("feat/other", sha, forge.StateMerged), wtLogger()); err != nil {
		t.Fatal(err)
	}
	if !wtExists(wt) {
		t.Error("unrelated worktree removed")
	}
}

func TestWorktreeHostileBranch(t *testing.T) {
	repo := wtRepo(t)
	wt := wtAddWorktree(t, repo, "feat/x")
	sha := wtGit(t, wt, "rev-parse", "HEAD")
	pwn := filepath.Join(t.TempDir(), "pwn")
	for _, b := range []string{"--force", "x;touch " + pwn, "$(id)", "`id`", "-f", "feat/x\n", "refs/heads/feat/x", "heads/feat/x"} {
		if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange(b, sha, forge.StateMerged), wtLogger()); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
	if !wtExists(wt) {
		t.Error("worktree removed by hostile branch")
	}
	if wtExists(pwn) {
		t.Error("hostile branch executed")
	}
}

func TestParseWorktreesPorcelainZ(t *testing.T) {
	in := "worktree /r/main\x00HEAD aaa\x00branch refs/heads/main\x00\x00" +
		"worktree /r/with\nnewline\x00HEAD bbb\x00branch refs/heads/feat/nl\x00\x00" +
		"worktree /r/det\x00HEAD ccc\x00detached\x00\x00" +
		"worktree /r/bare\x00bare\x00\x00" +
		"worktree /r/lock\x00HEAD ddd\x00branch refs/heads/l\x00locked because\x00\x00" +
		"worktree /r/lock2\x00HEAD eee\x00branch refs/heads/l2\x00locked\x00\x00" +
		"worktree /r/prune\x00HEAD fff\x00branch refs/heads/p\x00prunable gitdir gone\x00\x00"
	got := parseWorktrees([]byte(in))
	want := []worktreeEntry{
		{Path: "/r/main", Head: "aaa", Branch: "refs/heads/main"},
		{Path: "/r/with\nnewline", Head: "bbb", Branch: "refs/heads/feat/nl"},
		{Path: "/r/det", Head: "ccc", Detached: true},
		{Path: "/r/bare", Bare: true},
		{Path: "/r/lock", Head: "ddd", Branch: "refs/heads/l", Locked: true},
		{Path: "/r/lock2", Head: "eee", Branch: "refs/heads/l2", Locked: true},
		{Path: "/r/prune", Head: "fff", Branch: "refs/heads/p", Prunable: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestParseWorktreesNoTrailingSeparator(t *testing.T) {
	got := parseWorktrees([]byte("worktree /a\x00HEAD x\x00branch refs/heads/b\x00"))
	if len(got) != 1 || got[0].Path != "/a" || got[0].Branch != "refs/heads/b" {
		t.Errorf("got %+v", got)
	}
}

func TestWorktreeNewlinePathRemoved(t *testing.T) {
	repo := wtRepo(t)
	p := filepath.Join(filepath.Dir(repo), "nl\nwt")
	wtGit(t, repo, "worktree", "add", "-b", "feat/nl", p)
	sha := wtGit(t, p, "rev-parse", "HEAD")
	if err := removeMergedWorktree(context.Background(), wtRule(repo), wtChange("feat/nl", sha, forge.StateMerged), wtLogger()); err != nil {
		t.Fatal(err)
	}
	if wtExists(p) {
		t.Error("newline-path worktree not removed")
	}
}

type wtSource struct {
	byBranch map[string][]forge.Change
	errs     map[string]error
	asked    []string
}

func (s *wtSource) Subscribe(context.Context, client.SubscribeOptions) (<-chan forge.ChangeEvent, error) {
	return nil, errors.New("unused")
}

func (s *wtSource) Change(context.Context, forge.RepoRef, int, client.ReadOptions) (forge.Change, error) {
	return forge.Change{}, errors.New("unused")
}

func (s *wtSource) ByBranch(_ context.Context, _ forge.RepoRef, branch string, _ client.ReadOptions) ([]forge.Change, error) {
	s.asked = append(s.asked, branch)
	if err := s.errs[branch]; err != nil {
		return nil, err
	}
	return s.byBranch[branch], nil
}

func TestReconcileWorktrees(t *testing.T) {
	repo := wtRepo(t)
	merged := wtAddWorktree(t, repo, "feat/merged")
	open := wtAddWorktree(t, repo, "feat/open")
	boom := wtAddWorktree(t, repo, "feat/boom")
	other := wtAddWorktree(t, repo, "feat/other")
	late := wtAddWorktree(t, repo, "feat/late")
	sha := func(p string) string { return wtGit(t, p, "rev-parse", "HEAD") }

	theirs := wtChange("feat/other", sha(other), forge.StateMerged)
	theirs.Author.Login = "someone"
	src := &wtSource{
		byBranch: map[string][]forge.Change{
			"feat/merged": {wtChange("feat/merged", sha(merged), forge.StateMerged)},
			"feat/open":   {wtChange("feat/open", sha(open), forge.StateOpen)},
			"feat/other":  {theirs},
			"feat/late":   {wtChange("feat/late", sha(late), forge.StateMerged)},
		},
		errs: map[string]error{"feat/boom": errors.New("daemon down")},
	}
	err := reconcileWorktrees(context.Background(), wtRule(repo), src, "ME", wtLogger())
	if err == nil || !strings.Contains(err.Error(), "feat/boom") {
		t.Fatalf("err = %v, want one naming feat/boom", err)
	}
	for p, wantGone := range map[string]bool{merged: true, late: true, open: false, boom: false, other: false} {
		if wtExists(p) == wantGone {
			t.Errorf("%s: exists=%v, want gone=%v", filepath.Base(p), wtExists(p), wantGone)
		}
	}
	if slices.Contains(src.asked, "main") {
		t.Error("main worktree queried")
	}
	for _, b := range []string{"feat/merged", "feat/open", "feat/boom", "feat/other", "feat/late"} {
		if !slices.Contains(src.asked, b) {
			t.Errorf("branch %s not queried (%v)", b, src.asked)
		}
	}
}

func TestReconcileWorktreesCancelled(t *testing.T) {
	repo := wtRepo(t)
	wtAddWorktree(t, repo, "feat/x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &wtSource{}
	if err := reconcileWorktrees(ctx, wtRule(repo), src, "me", wtLogger()); err == nil {
		t.Error("want context error")
	}
	if len(src.asked) != 0 {
		t.Errorf("queried after cancel: %v", src.asked)
	}
}

func TestReconcileWorktreesContinuesAfterRemoveError(t *testing.T) {
	repo := wtRepo(t)
	bad := wtAddWorktree(t, repo, "feat/bad")
	good := wtAddWorktree(t, repo, "feat/good")
	sha := func(p string) string { return wtGit(t, p, "rev-parse", "HEAD") }
	badSHA := sha(bad)
	// A broken .git file makes `git status` in the worktree fail.
	if err := os.WriteFile(filepath.Join(bad, ".git"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &wtSource{byBranch: map[string][]forge.Change{
		"feat/bad":  {wtChange("feat/bad", badSHA, forge.StateMerged)},
		"feat/good": {wtChange("feat/good", sha(good), forge.StateMerged)},
	}}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	if err := reconcileWorktrees(context.Background(), wtRule(repo), src, "me", log); err != nil {
		t.Fatalf("err = %v, want nil: a permanent removal failure must not trigger a retry", err)
	}
	if !strings.Contains(logs.String(), "removing worktree failed") || !strings.Contains(logs.String(), "feat/bad") {
		t.Errorf("removal failure not logged with its branch: %s", logs.String())
	}
	if wtExists(good) {
		t.Error("good worktree not removed after an earlier failure")
	}
	if !wtExists(bad) {
		t.Error("broken worktree unexpectedly removed")
	}
}
