# forged

A local daemon that keeps one shared copy of pull/merge-request state from
code forges and serves it to local tools. Shells, editors, dashboards and
agents all ask the same question ("what is the state of PR 7?") many times a
minute; forged answers from memory, batches and coalesces the upstream
calls, and keeps the data fresh in the background only while someone is reading
it.

| Without it | With it |
| --- | --- |
| Every tool polls the forge on its own | One daemon fetches, everyone reads the same copy |
| Duplicate requests for the same PR or repo | Same-key and same-repo reads are coalesced and batched |
| Rate limits burn on idle polling | Only recently read entries are refreshed; a rate limit pauses the provider |

Only GitHub (`github.com`) is implemented. The model and provider interface are
forge-agnostic.

## Install

Flake package:

```fish
nix build github:noamsto/forged
nix run github:noamsto/forged -- version
```

Home Manager module (`flake.homeManagerModules.default`):

```nix
{
  imports = [ forged.homeManagerModules.default ];
  services.forged = {
    enable = true;
    # package = ...;     # default: this flake's package
    # ghPackage = pkgs.gh; # put on the daemon's PATH
    # extraArgs = [ "--lease-ttl" "20m" ];  # appended to `forged serve`
  };
}
```

The module installs the package and defines the user socket and service units.
For the hooks runner see [Hooks](#hooks).

Manual units: the package ships `share/systemd/user/forged.{socket,service}`
(and `forged-hooks.service`, see [Hooks](#hooks)).
Link them into `~/.config/systemd/user/`, then:

```fish
systemctl --user daemon-reload
systemctl --user enable --now forged.socket
```

The daemon calls the GitHub API using the token from `gh`, so `gh` must be on
its `PATH` and authenticated.

## Socket activation

The socket unit listens on `$XDG_RUNTIME_DIR/forged/forged.sock`
(`SocketMode=0600`, `DirectoryMode=0700`); systemd starts the daemon on the first
connection. Without activation, `forged serve` listens on `--socket`, else
the same default path (an error if `XDG_RUNTIME_DIR` is unset). A stale socket
file that nothing answers on is removed; `--socket` pointing at anything that is
not a socket is refused, and the socket is created `0600`. Clients resolve the
path from `FORGED_SOCKET`, else the default; with neither `FORGED_SOCKET` nor
`XDG_RUNTIME_DIR` set the client refuses to run (`forged pr` then falls back to
`gh`).

The daemon saves a snapshot of its entries every 60s and on shutdown
(`SIGINT`/`SIGTERM`) to `$XDG_STATE_HOME/forged/snapshot.json` (fallback
`~/.local/state`), and loads it at start. Loaded entries are cold until read, and
their max-age is measured from the original fetch time.

## Freshness

| Rule | Value |
| --- | --- |
| Read max-age | 30s by default; `max_age=0` always fetches |
| Negative result (not found, empty branch lookup) | max-age capped at 15s |
| Terminal change (merged or closed) | max-age at least 1h; refreshed every 1h |
| Lease | A read leases an entry for 10m; unleased entries stop being refreshed |
| Hot (open, leased, checks pending or mergeable unknown, focused, or invalidated within 5m) | Refetched every 25s; all due hot changes share one batched call |
| Warm (other leased entries) | Repo probed every 2m with a conditional request; refetched only if it changed |
| Warm hard refresh | Refetched in full at least every 10m, covering state the probe cannot see |
| Cold (unleased) | Not polled; evicted after 1h without a read |
| Search queries | No probe; refetched every 2m while leased |
| Rate limit | The provider is paused until its retry time; reads return the error, polling stops |
| Failed poll | Exponential backoff, up to 10m |

Caveats and controls:

- A closed PR that is reopened is picked up when the repo's open list is next
  fetched or probed, or by `invalidate`.
- `invalidate` on a change refetches it now and keeps it hot for 5m. `invalidate`
  on a repo refetches the repo's open list and marks the repo's cached changes
  stale, so the next read or tick refetches them instead of all being refetched
  eagerly.
- `focus=1` on a read keeps a change hot for the lease.
- `serve` flags set the lease and the hot and warm intervals; other durations are
  fixed defaults.

## Daemon HTTP API

JSON over the unix socket. `repo` is `host/owner/name`, for example
`github.com/o/r`.

| Route | Parameters | Returns |
| --- | --- | --- |
| `GET /v1/changes` | `repo`; optional `head` (branch) | Open changes of the repo, or the changes of that branch |
| `GET /v1/change` | `repo`, `number` (1 to 2147483647) | One change |
| `GET /v1/search` | `forge`, `q`; optional `limit` (default 30, 1..100) | Changes matching a forge search query |
| `POST /v1/invalidate` | JSON body `{"key": <forge.Key>}`; the key is validated (kind, repo, number range, branch, query, limit) | `{}` after the refetch |
| `GET /v1/status` | none | `{"version", "stats"}` |
| `GET /v1/watch` | `repo`; optional `number` (omit for the whole repo), `types` (comma-separated event types), `since` (sequence number) | Newline-delimited JSON stream of change events (no state snapshot) |

The read routes (`changes`, `change`, `search`) also take `max_age` (a Go duration
such as `1m`, or integer seconds) and `focus=1`. They answer
`{"changes": [...], "fetchedAt": "...", "negative": false}`.

Errors are `{"error": "..."}` with these codes:

| Code | Meaning |
| --- | --- |
| 400 | Bad or missing parameter, invalid body |
| 404 | Change not found |
| 501 | Operation unsupported by the provider |
| 502 | Upstream or provider failure (including rate-limit pause) |

```
curl --unix-socket $XDG_RUNTIME_DIR/forged/forged.sock 'http://x/v1/change?repo=github.com/o/r&number=1'
```

## CLI

```
forged serve [--socket PATH] [--snapshot PATH] [--lease-ttl D] [--hot-interval D] [--warm-interval D]
forged pr view|list ...        # gh-compatible subset
forged invalidate <host/owner/repo>[#N]
forged watch [-R owner/repo] [--pr N] [--event t1,t2] [--since SEQ]
forged hooks [--config PATH]   # run local commands on change events
forged status                  # daemon version and stats, JSON
forged version
```

`invalidate github.com/o/r#5` refetches one change; `invalidate github.com/o/r`
invalidates the repo (see Freshness).

### `pr` is a drop-in for a subset of `gh pr`

Output is shaped like `gh`'s `--json`. Anything outside the subset is handed to
the real `gh` unchanged (it replaces the process via exec), so you can alias
`gh pr` to it.

| Supported | Notes |
| --- | --- |
| `pr view <number or https://github.com/o/r/pull/N>` | `--json` with fields is required |
| `pr list` | open PRs by default; `--head`, `--state open\|closed\|merged\|all`, `--limit` (default 30) |
| `-R/--repo`, `--json`, `--jq/-q` | `-H`, `-s`, `-L` are the short forms of `--head`, `--state`, `--limit` |
| `--max-age DURATION` | forged only; also `FORGED_MAX_AGE` |

`--json` fields: `number`, `url`, `title`, `state`, `isDraft`, `headRefName`,
`headRefOid`, `baseRefName`, `mergeable`, `mergeStateStatus`, `reviewDecision`,
`statusCheckRollup`, `autoMergeRequest`, `updatedAt`, `author`.

Falls back to `gh` when:

- the command is not `pr view` or `pr list`, or a flag, field or target is not in
  the table above;
- `--state` other than `open` is used with `list` but without `--head`, `--head`
  is combined with `--limit` above 100, or `--state`, `--head` or `--limit` is
  passed to `view`;
- the repository cannot be resolved to a single `github.com` remote (several
  remotes, or `gh repo set-default`), or the host is a GitHub Enterprise host;
- the daemon is unavailable, answers non-2xx, or the `--jq` expression fails to
  compile or fails at run time. Output is written only after jq succeeds, so a
  fallback never repeats lines.

The fallback drops `--max-age`, which `gh` does not know.

`FORGED_NO_FALLBACK=1` disables the fallback and reports the error instead.

Divergences from `gh`: output is indented on a TTY but not colored; empty
lists are `[]` rather than `null`; multi-remote repositories are not resolved the
way `gh` does it.

## Watching changes

`forged watch` prints one JSON change event per line until interrupted. The
repo comes from `-R`, `GH_REPO` or the git remote; `--pr N` narrows to one PR,
`--event` to a list of types, and `--since SEQ` replays buffered events after
that sequence number first. There is no `gh` fallback. Exit codes: 0 on
interrupt, 1 when the daemon is unavailable or the stream ended, 2 on bad usage.
When the stream ends after events (or with `--since`), stderr prints the
`--since` to reconnect with; if it ended before any event, stderr says
`stream ended before any event` with no resume hint.

Events carry `seq`, `type`, `repo`, `number`, `url`, `old`, `new`, `at` and, for
`review.submitted`, `review`.

| Type | Emitted when |
| --- | --- |
| `checks.started` | the check rollup becomes pending |
| `checks.completed` | the rollup is pass or fail and differs from before, or the head changed |
| `review.submitted` | a new non-pending review appears |
| `review_decision.changed` | the review decision changes |
| `state.changed` | open, closed or merged changes (a reopen reports `open`) |
| `draft.changed` | the PR moves between draft and ready |
| `head.changed` | the head commit changes |
| `mergeable.changed` | mergeability changes |
| `resync` | stream marker, not a change: the cursor fell out of the buffer |

Rules for consumers:

- Delivery is at-least-once. A consumer that falls too far behind has its stream
  closed; reconnect with the last `seq`.
- Re-read the PR (`--max-age 0`) after every (re)connect and after a `resync`:
  events are produced by diffing polls, so anything missed shows only in a fresh
  read.
- A `checks.completed` can be followed by `checks.started` when slower workflows
  register after a push. Act on the final `--max-age 0` read, not on the first
  `checks.completed`.
- A repo-wide watch (no `--pr`) hot-polls every open PR of the repo, one batched
  fetch every 25s. A newly opened PR emits no event, since it has no baseline;
  re-list the repo on reconnect.

## Hooks

`forged hooks` runs local commands when a change event matches a rule, for
chores such as a desktop notification on a failed check or removing the
worktree of a merged PR. It is a separate subscriber process with its own unit
that uses the same `Subscribe` API as `forged watch`. The daemon never runs
commands. Hooks act on the host they run on: each machine runs its own
`forged hooks` against its own daemon and config.

```toml
viewer = "me"   # default: `gh api user --jq .login`

[[rule]]
name = "notify-ci-failed"
repos = ["me/app"]
on = "checks.completed"
conclusion = "fail"
branch = "feat/*"
command = ["notify-send", "CI failed", "--", "{repo}#{number} {title}"]

[[rule]]
name = "cleanup-merged"
repos = ["me/app"]
builtin = "remove-merged-worktree"
checkout = "~/git/me/app"
```

### Config

`--config PATH`, default `$XDG_CONFIG_HOME/forged/hooks.toml`, else
`~/.config/forged/hooks.toml`. Unknown keys are rejected. The runner needs a
running daemon (socket activation starts it).

Top level: `viewer`, the login "my PRs" means. Default: `gh api user --jq .login`,
resolved at start (an error if a rule needs it and it cannot be resolved).
Logins compare case-insensitively.

Each `[[rule]]`:

| Key | Meaning |
| --- | --- |
| `name` | Label for logs |
| `repos` | Required list of `owner/name`. This is also the subscription set: events of other repos are never seen |
| `on` | Event type (see [Watching changes](#watching-changes)). Required for `command` rules, forbidden for builtins |
| `from`, `to` | Match the event's `old` / `new` value, for example `to = "merged"`. Validated at load against the values that event type can carry: for example `checks.started` only has `to = "pending"`, `checks.completed` only `pass` or `fail`, and `review.submitted` has no `from` |
| `conclusion` | `pass` or `fail`; only with `on = "checks.completed"`, matches the event's `new` |
| `author` | PR author login. Default: the viewer. `"*"` = anyone |
| `branch` | Glob on the PR's head branch; `*` matches any run of characters including `/`, `?` one character |
| `command` | Argv array, run without a shell. Exactly one of `command` and `builtin` |
| `builtin` | `remove-merged-worktree` |
| `checkout` | Builtin only: absolute path (a leading `~` is expanded) of a clone of the repo. Never checked at load, only when a rule runs |
| `timeout` | `command` rules only (rejected on builtins). Go duration, default `60s`. The command is killed when it expires |

Events carry no branch or author, so for each event the runner reads the change
through the daemon (a read at most 1h old) and filters on that. If the read
fails the event is logged and dropped.
Rules run one at a time, in order.

### Placeholders

Replaced inside a single `command` element, so `"{repo}#{number}"` is one
argument:

| Token | Value |
| --- | --- |
| `{repo}` | `host/owner/name` |
| `{number}` | PR number |
| `{url}` | PR URL |
| `{type}` | Event type |
| `{old}`, `{new}` | The event's old and new value |
| `{branch}` | Head branch |
| `{base}` | Base branch |
| `{sha}` | Head commit |
| `{title}` | PR title |
| `{author}` | PR author login |

Other brace text (jq programs, `{{.Name}}`) is left alone. An unknown
lowercase `{word}` is a config error, to catch typos. There is no escape syntax.

### Environment and stdin

The command gets `FORGED_EVENT_TYPE`, `FORGED_REPO`, `FORGED_NUMBER`,
`FORGED_URL`, `FORGED_OLD`, `FORGED_NEW`, `FORGED_BRANCH`, `FORGED_BASE`,
`FORGED_SHA`, `FORGED_TITLE` and `FORGED_AUTHOR`. Inherited `FORGED_*`
variables are removed, except `FORGED_SOCKET`, so the command reaches the same
daemon. Stdin is JSON, the event and the change as the daemon serves them:

```json
{"event": {"seq": 1, "type": "state.changed", "repo": {}, "number": 7, "old": "open", "new": "merged"}, "change": {}}
```

The command's output is captured and capped. It is logged at warn when the
command fails; success is logged at debug level only. A failing command is not
retried.

### Safety model

Branch names, titles and authors come from the forge and are attacker
controlled (anyone who can open a PR picks them).

- `command` is an argv array run directly, never through a shell.
- Substitution is one non-recursive pass inside an element, so a value never
  adds arguments and is never expanded again. `command[0]` cannot contain a
  placeholder.
- A value is data, but the program may still treat it as an option. If an
  element starts with a placeholder and expands to something starting with `-`,
  the run is refused, unless a literal `--` element comes earlier. Put `--`
  before placeholders where the program supports it, or prefix the element
  (`--title={title}`).
- Passing a value to `sh -c` or `eval` re-introduces a shell. Read it from
  `"$1"` or the environment, never splice it into the script.
- Rules match only your own PRs by default (`author` = viewer). Use
  `author = "*"` deliberately.

### Built-in: `remove-merged-worktree`

Fixed to `state.changed` to `merged`; `on`, `from`, `to`, `conclusion` and
`command` are not allowed. Needs `checkout`, a clone of the repo, and exactly one
entry in `repos`. It removes the worktree of `checkout` that has the merged
PR's head branch checked out, but only when all of these hold:

- the worktree is not the main one, and is not locked or detached;
- its `HEAD` is the PR's head commit;
- `git status --porcelain --untracked-files=all` is empty.

Otherwise it keeps the worktree and logs why. The removal is a plain
`git worktree remove` without `--force`, so ignored files are deleted along with
the worktree. A PR whose head was updated on the web after your last push keeps
its worktree on purpose (the heads differ), since the local branch may lack
those commits. The branch name is only compared in memory, never given to git.

### Reconcile

Events are edges, so a merge while the runner is down would be missed. Builtin
rules are therefore also level-triggered: at start, after every reconnect
without a cursor, and on a `resync` event, the runner checks each worktree of
`checkout` and removes those whose branch has a merged PR passing the rule's
filters. Only this reconcile pass is retried until it succeeds. Delivery to hooks is
at-most-once: an event whose change read or command fails is logged and
dropped, and `command` rules are edge-only, so expect no catch-up for events
missed while stopped or failed.

### Install

Home Manager (needs `services.forged.enable`):

```nix
services.forged.hooks = {
  enable = true;
  settings = {
    viewer = "me";
    rule = [
      {
        name = "cleanup-merged";
        repos = [ "me/app" ];
        builtin = "remove-merged-worktree";
        checkout = "~/git/me/app";
      }
    ];
  };
  extraPackages = [ pkgs.libnotify ];  # on the runner's PATH, with gh and git
  # gitPackage = pkgs.git;
};
```

The module writes `~/.config/forged/hooks.toml`, defines `forged-hooks.service`
(requires `forged.socket`, restarts on config change) and puts `gh`, `git` and
`extraPackages` on its `PATH`. The unit restarts on failure, but not on exit
status 2 (bad config or usage), which a restart cannot fix. Config is read once
at startup, so restart the service after editing it (Home Manager restarts it on
change).

Manually: link `share/systemd/user/forged-hooks.service` into
`~/.config/systemd/user/`, then:

```fish
systemctl --user daemon-reload
systemctl --user enable --now forged-hooks.service
```

The unit's `PATH` must contain `gh`, `git` and whatever your commands run. After
editing the config, run `systemctl --user restart forged-hooks`.

## Agent skill

[`skills/forged/SKILL.md`](skills/forged/SKILL.md) teaches agents to
read PR state through `forged pr`, to wait with `forged watch`
instead of polling, and to point at `forged hooks` for cleanup chores. The flake installs it two ways:

- `nix build .#skill` puts `SKILL.md` at the root of the output.
- `nix build .#forged` includes it at
  `share/forged/skills/forged/SKILL.md`.

## Go client

```go
cl := client.New("") // "" = $FORGED_SOCKET or the default socket
maxAge := time.Minute
repo := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}

c, err := cl.Change(ctx, repo, 7, client.ReadOptions{MaxAge: &maxAge})
if errors.Is(err, client.ErrUnavailable) {
	// daemon not running: go to the forge directly
}
```

`Change`, `ListOpen`, `ByBranch`, `Search`, `Invalidate`, `Status` and `Subscribe`
(a channel of `forge.ChangeEvent`; number 0 watches the whole repo) map to the routes
above. `ErrNotFound` means 404; other non-2xx responses are `*APIError`.

## Out of scope

- `Search` (`/v1/search`, `client.Search`) exists only in the daemon and client;
  there is no CLI for it.
- GitHub Enterprise hosts: the provider serves `github.com` only, and the CLI
  falls back to `gh` for other hosts.
- `gh event listen`: the provider reports an events capability flag, but no push
  event source is implemented.
- Forge writes and mutations (hooks run local commands only), PR details, threads and files, remote daemons, other
  forges.

## Development

```fish
nix develop                                   # dev shell with Go and tools
nix develop -c go test -race ./...
nix fmt                                       # treefmt
nix build -L .#checks.x86_64-linux.go-gate    # the CI gate
```
