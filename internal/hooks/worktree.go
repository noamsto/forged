package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/noamsto/forged/client"
	"github.com/noamsto/forged/forge"
)

type worktreeEntry struct {
	Path     string
	Head     string
	Branch   string // full ref, "" when detached or bare
	Bare     bool
	Detached bool
	Locked   bool
	Prunable bool
}

// parseWorktrees parses `git worktree list --porcelain -z`: NUL-terminated
// fields, with an empty field ending each entry.
func parseWorktrees(out []byte) []worktreeEntry {
	var entries []worktreeEntry
	var cur worktreeEntry
	var open bool
	flush := func() {
		if open {
			entries = append(entries, cur)
		}
		cur, open = worktreeEntry{}, false
	}
	for field := range bytes.SplitSeq(out, []byte{0}) {
		if len(field) == 0 {
			flush()
			continue
		}
		key, val, _ := strings.Cut(string(field), " ")
		switch key {
		case "worktree":
			flush()
			cur.Path, open = val, true
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = val
		case "bare":
			cur.Bare = true
		case "detached":
			cur.Detached = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	flush()
	return entries
}

// listWorktrees returns the linked worktrees of checkout; the main worktree
// (always first) is dropped.
func listWorktrees(ctx context.Context, checkout string) ([]worktreeEntry, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", checkout, "worktree", "list", "--porcelain", "-z").Output() //nolint:gosec // argv-only, no shell; path is operator config
	if err != nil {
		return nil, fmt.Errorf("git worktree list in %s: %w", checkout, err)
	}
	entries := parseWorktrees(out)
	if len(entries) == 0 {
		return nil, nil
	}
	return entries[1:], nil
}

// removeMergedWorktree removes the worktree of checkout that has the merged
// change's head branch checked out, unless it is dirty, locked or not at the
// PR head. The branch name is only compared in-process, never passed to git.
func removeMergedWorktree(ctx context.Context, r Rule, ch forge.Change, log *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := listWorktrees(ctx, r.Checkout)
	if err != nil {
		return err
	}
	want := "refs/heads/" + ch.Head.Ref
	for _, wt := range entries {
		if wt.Bare || wt.Detached || wt.Prunable || wt.Branch != want {
			continue
		}
		return removeWorktree(ctx, r.Checkout, wt, ch, log.With("rule", r.Name, "path", wt.Path, "branch", ch.Head.Ref))
	}
	return nil
}

func removeWorktree(ctx context.Context, checkout string, wt worktreeEntry, ch forge.Change, log *slog.Logger) error {
	if wt.Locked {
		log.Info("keeping worktree: locked")
		return nil
	}
	if wt.Head != ch.Head.SHA {
		log.Info("keeping worktree: HEAD differs from the merged PR head", "head", wt.Head, "pr_head", ch.Head.SHA)
		return nil
	}
	status, err := exec.CommandContext(ctx, "git", "-C", wt.Path, "status", "--porcelain", "--untracked-files=all").Output() //nolint:gosec // argv-only, no shell; path comes from git's own worktree list
	if err != nil {
		return fmt.Errorf("git status in %s: %w", wt.Path, err)
	}
	if len(bytes.TrimSpace(status)) > 0 {
		log.Info("keeping worktree: uncommitted or untracked files")
		return nil
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", checkout, "worktree", "remove", "--", wt.Path).CombinedOutput(); err != nil { //nolint:gosec // argv-only, no shell; path comes from git's own worktree list
		return fmt.Errorf("git worktree remove %s: %w: %s", wt.Path, err, bytes.TrimSpace(out))
	}
	log.Info("removed worktree of merged change")
	return nil
}

// reconcileWorktrees removes every worktree of r.Checkout whose branch has a
// merged change matching the rule, catching merges missed while not running.
func reconcileWorktrees(ctx context.Context, r Rule, src Source, viewer string, log *slog.Logger) error {
	if len(r.Repos) != 1 {
		return fmt.Errorf("rule %q: builtin needs exactly one repo", r.Name)
	}
	entries, err := listWorktrees(ctx, r.Checkout)
	if err != nil {
		return err
	}
	var errs []error
	for _, wt := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		branch, ok := strings.CutPrefix(wt.Branch, "refs/heads/")
		if !ok || wt.Bare || wt.Detached || wt.Prunable {
			continue
		}
		changes, err := src.ByBranch(ctx, r.Repos[0], branch, client.ReadOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("reconcile: reading changes by branch failed", "rule", r.Name, "branch", branch, "err", err)
			errs = append(errs, fmt.Errorf("branch %s: %w", branch, err))
			continue
		}
		for _, ch := range changes {
			if ch.State != forge.StateMerged || !r.MatchChange(ch, viewer) {
				continue
			}
			if err := removeMergedWorktree(ctx, r, ch, log); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Not returned: a local removal failure never self-resolves, and a
				// returned error would make the runner retry (and re-query the forge) forever.
				log.Warn("reconcile: removing worktree failed", "rule", r.Name, "branch", branch, "err", err)
			}
		}
	}
	return errors.Join(errs...)
}
