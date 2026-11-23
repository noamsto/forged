---
name: forge-cache
description: Use when reading pull request state, checks, reviews or mergeability, or waiting for CI or reviews. Serves `gh pr view|list` from a shared local cache and streams change events.
---

# forge-cache

A local daemon holds one shared copy of PR state. `forge-cache pr` answers
from it, and `forge-cache watch` pushes changes, so neither you nor other
agents poll GitHub.

## Reading

Use `forge-cache pr view|list … --json …` wherever you would use `gh pr view|list`. Flags and
JSON shape match `gh`.

```fish
forge-cache pr view 12 -R noamsto/forge-cache --json state,statusCheckRollup,reviewDecision,mergeable
forge-cache pr list -R noamsto/forge-cache --json number,title,headRefName
```

- Fallback to `gh` is automatic for anything outside the cached subset or when the
  daemon is down. Call `forge-cache` first, never `gh` "to be safe".
- Default freshness (30s) suits exploring. Add `--max-age 0` on the read that
  precedes an action: a merge, a status report, a next-step decision.

```fish
forge-cache pr view 12 -R noamsto/forge-cache --json statusCheckRollup,mergeable --max-age 0
```

## Writing

Mutations stay on `gh` (merge, comment, edit, push). Right after one, refetch the
change so the next reader sees it:

```fish
forge-cache invalidate github.com/noamsto/forge-cache#12
```

`invalidate github.com/noamsto/forge-cache` refetches the repo's open list.

## Waiting for CI or reviews

Watch instead of polling. Order matters, so nothing slips between read and watch:

1. Start the watch, in the background so it keeps running.
2. Read the PR with `--max-age 0`. If checks are no longer pending (or the review
   you await is in), stop: there is nothing to wait for.
3. Otherwise block on the watch output until an event you await arrives.
4. Read again with `--max-age 0` and act on that read, never on the event alone.

```fish
forge-cache watch -R noamsto/forge-cache --pr 12 --event checks.completed,review.submitted,review_decision.changed
```

Event types: `checks.started`, `checks.completed`, `review.submitted`,
`review_decision.changed`, `state.changed`, `draft.changed`, `head.changed`,
`mergeable.changed`. Each line is JSON with `seq`, `type`, `number`, `old`, `new`.

A `checks.completed` can be followed by `checks.started` when slower workflows
register after a push. The final `--max-age 0` read decides whether CI is done.

### Reconnect

Exit code 1 means the stream ended. stderr names the last `seq`; resume from it:

```fish
forge-cache watch -R noamsto/forge-cache --pr 12 --event checks.completed --since 1769990000123456
```

After every reconnect, and after a `resync` event (the cursor fell out of the
buffer), re-read with `--max-age 0` before trusting your picture of the PR.

### Repo-wide watch

Omitting `--pr` watches every open PR in the repo. It hot-polls them all, so use it
only when you need the whole repo. A newly opened PR emits no event; list the repo
again on reconnect to find it.

```fish
forge-cache watch -R noamsto/forge-cache --event state.changed,head.changed
```
