---
name: forged
description: Use when reading pull request state, checks, reviews or mergeability, or waiting for CI or reviews, or when configuring `forged hooks` to run local commands on PR events (e.g. remove a merged PR's worktree). Serves `gh pr view|list` from a shared local cache and streams change events.
---

# forged

A local daemon holds one shared copy of PR state. `forged pr` answers
from it, and `forged watch` pushes changes, so neither you nor other
agents poll GitHub.

## Reading

Use `forged pr view|list … --json …` wherever you would use `gh pr view|list`. Flags and
JSON shape match `gh`.

```fish
forged pr view 12 -R noamsto/forged --json state,statusCheckRollup,reviewDecision,mergeable
forged pr list -R noamsto/forged --json number,title,headRefName
```

- Fallback to `gh` is automatic for anything outside the cached subset or when the
  daemon is down. Call `forged` first, never `gh` "to be safe".
- Default freshness (30s) suits exploring. Add `--max-age 0` on the read that
  precedes an action: a merge, a status report, a next-step decision.

```fish
forged pr view 12 -R noamsto/forged --json statusCheckRollup,mergeable --max-age 0
```

## Writing

Mutations stay on `gh` (merge, comment, edit, push). Right after one, refetch the
change so the next reader sees it:

```fish
forged invalidate github.com/noamsto/forged#12
```

`invalidate github.com/noamsto/forged` refetches the repo's open list.

## Waiting for CI or reviews

Watch instead of polling. Order matters, so nothing slips between read and watch:

1. Start the watch, in the background so it keeps running.
2. Read the PR with `--max-age 0`. If checks are no longer pending (or the review
   you await is in), stop: there is nothing to wait for.
3. Otherwise block on the watch output until an event you await arrives.
4. Read again with `--max-age 0` and act on that read, never on the event alone.

```fish
forged watch -R noamsto/forged --pr 12 --event checks.completed,review.submitted,review_decision.changed
```

Event types: `checks.started`, `checks.completed`, `review.submitted`,
`review_decision.changed`, `state.changed`, `draft.changed`, `head.changed`,
`mergeable.changed`. Each line is JSON with `seq`, `type`, `number`, `old`, `new`.

A `checks.completed` can be followed by `checks.started` when slower workflows
register after a push. The final `--max-age 0` read decides whether CI is done.

### Reconnect

Exit code 1 has three causes, told apart by the last stderr line:

- `stream ended after seq N; reconnect with --since N`: resume from that seq.
- `stream ended before any event`: the daemon was reachable (it may have
  restarted) but no cursor exists. Rerun the same watch without `--since`, then
  redo the read step.
- Any other `forged: …` error (a dial failure): the daemon is unavailable.
  `watch` has no `gh` fallback, so poll `forged pr view … --max-age 0` every few
  tens of seconds until the condition holds, and retry `watch` now and then.

Resume:

```fish
forged watch -R noamsto/forged --pr 12 --event checks.completed --since 1769990000123456
```

After every reconnect, and after a `resync` event (the cursor fell out of the
buffer), re-read with `--max-age 0` before trusting your picture of the PR.

### Repo-wide watch

Omitting `--pr` watches every open PR in the repo. It hot-polls them all, so use it
only when you need the whole repo. A newly opened PR emits no event; list the repo
again on reconnect to find it.

```fish
forged watch -R noamsto/forged --event state.changed,head.changed
```

## Reacting to events with hooks

For a recurring chore tied to an event (notify on a failed check, remove the
worktree of a merged PR), do not build a polling loop or a long-lived watch.
`forged hooks` runs a local command per matching event, from rules in
`~/.config/forged/hooks.toml`. The user runs it as a service; your job is to
propose or edit the config, and to say so if a rule would help. The service
already runs `forged hooks`: do not start a second copy. To try a config by hand,
stop the service first.

```toml
[[rule]]
name = "cleanup-merged"
repos = ["noamsto/forged"]
builtin = "remove-merged-worktree"
checkout = "~/git/noamsto/forged"
```

```fish
forged hooks --config ~/.config/forged/hooks.toml
```

- Under Home Manager the config file is generated: edit
  `services.forged.hooks.settings` instead. Otherwise, after editing run
  `systemctl --user restart forged-hooks`; the runner reads the config only at
  startup.
- Rules match only the viewer's own PRs unless `author = "*"`. `repos` is
  required.
- `command` is an argv array, no shell. A run is refused whenever an element
  beginning with a placeholder expands to a value starting with `-`, checked per
  event, not at config load. Put a literal `--` before such elements
  (`{branch}`).
- Event data is attacker controlled: never splice it into `sh -c` scripts.
- The builtin keeps dirty, untracked, locked or moved-head worktrees, and never
  uses `--force`.
