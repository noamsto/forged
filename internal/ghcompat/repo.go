package ghcompat

import (
	"context"
	"errors"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

const gitTimeout = 5 * time.Second

const githubPrefix = "https://github.com/"

var (
	// matches https://github.com/o/r(.git), ssh://git@github.com/o/r and git@github.com:o/r
	remoteRe = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)
)

// ResolveRepo picks the repo for req the way gh does when it can be decided
// without gh's own multi-remote logic: -R, a URL argument, GH_REPO, then the
// single GitHub remote of the git repo in dir.
func ResolveRepo(ctx context.Context, req Request, env func(string) string, dir string) (forge.RepoRef, *Fallback) {
	switch {
	case req.Repo != "":
		return parseRepo(req.Repo)
	case req.URL != "":
		ownerName, _, _ := strings.Cut(strings.TrimPrefix(req.URL, githubPrefix), "/pull/")
		return parseRepo(ownerName)
	case env("GH_REPO") != "":
		return parseRepo(env("GH_REPO"))
	}
	return repoFromGit(ctx, dir)
}

func parseRepo(s string) (forge.RepoRef, *Fallback) {
	ref, err := forge.ParseRepo(s)
	if err != nil {
		return forge.RepoRef{}, fallbackf("%v", err)
	}
	return ref, nil
}

func repoFromGit(ctx context.Context, dir string) (forge.RepoRef, *Fallback) {
	resolved, err := git(ctx, dir, "config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 1: // no match
	case err != nil:
		return forge.RepoRef{}, fallbackf("git config: %v", err)
	case strings.TrimSpace(resolved) != "":
		return forge.RepoRef{}, fallbackf("gh default repo is set")
	}

	out, err := git(ctx, dir, "remote", "-v")
	if err != nil {
		return forge.RepoRef{}, fallbackf("git remote: %v", err)
	}
	remotes := map[string]forge.RepoRef{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "(fetch)" {
			continue
		}
		if m := remoteRe.FindStringSubmatch(fields[1]); m != nil {
			remotes[fields[0]] = forge.RepoRef{Forge: "github", Host: "github.com", Owner: m[1], Name: m[2]}
		}
	}
	if len(remotes) != 1 {
		return forge.RepoRef{}, fallbackf("%d GitHub remotes, want exactly one", len(remotes))
	}
	return slices.Collect(maps.Values(remotes))[0], nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed git invocation
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
