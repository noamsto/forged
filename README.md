# forge-cache

A local daemon that keeps one shared copy of pull/merge-request state from
code forges and serves it to local tools. Shells, editors, dashboards and
agents all ask the same question ("what is the state of PR 7?") many times a
minute; forge-cache answers from memory, batches and coalesces the upstream
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
nix build github:noamsto/forge-cache
nix run github:noamsto/forge-cache -- version
```

Home Manager module (`flake.homeManagerModules.default`):

```nix
{
  imports = [ forge-cache.homeManagerModules.default ];
  services.forge-cache = {
    enable = true;
    # package = ...;     # default: this flake's package
    # ghPackage = pkgs.gh; # put on the daemon's PATH
    # extraArgs = [ "--lease-ttl" "20m" ];  # appended to `forge-cache serve`
  };
}
```

The module installs the package and defines the user socket and service units.

Manual units: the package ships `share/systemd/user/forge-cache.{socket,service}`.
Link them into `~/.config/systemd/user/`, then:

```fish
systemctl --user daemon-reload
systemctl --user enable --now forge-cache.socket
```

The daemon calls the GitHub API using the token from `gh`, so `gh` must be on
its `PATH` and authenticated.

## Socket activation

The socket unit listens on `$XDG_RUNTIME_DIR/forge-cache/forge-cache.sock`
(`SocketMode=0600`, `DirectoryMode=0700`); systemd starts the daemon on the first
connection. Without activation, `forge-cache serve` listens on `--socket`, else
the same default path (an error if `XDG_RUNTIME_DIR` is unset). A stale socket
file that nothing answers on is removed. Clients resolve the path from
`FORGE_CACHE_SOCKET`, else the default.

The daemon saves a snapshot of its entries every 60s and on shutdown
(`SIGINT`/`SIGTERM`) to `$XDG_STATE_HOME/forge-cache/snapshot.json` (fallback
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
| `GET /v1/change` | `repo`, `number` | One change |
| `GET /v1/search` | `forge`, `q`; optional `limit` | Changes matching a forge search query |
| `POST /v1/invalidate` | JSON body `{"key": <forge.Key>}` | `{}` after the refetch |
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
curl --unix-socket $XDG_RUNTIME_DIR/forge-cache/forge-cache.sock 'http://x/v1/change?repo=github.com/o/r&number=1'
```

## CLI

```
forge-cache serve [--socket PATH] [--snapshot PATH] [--lease-ttl D] [--hot-interval D] [--warm-interval D]
forge-cache pr view|list ...        # gh-compatible subset
forge-cache invalidate <host/owner/repo>[#N]
forge-cache watch [-R owner/repo] [--pr N] [--event t1,t2] [--since SEQ]
forge-cache status                  # daemon version and stats, JSON
forge-cache version
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
| `--max-age DURATION` | forge-cache only; also `FORGE_CACHE_MAX_AGE` |

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
  compile.

`FORGE_CACHE_NO_FALLBACK=1` disables the fallback and reports the error instead.

Divergences from `gh`: output is indented on a TTY but not colored; empty
lists are `[]` rather than `null`; multi-remote repositories are not resolved the
way `gh` does it.

## Watching changes

`forge-cache watch` prints one JSON change event per line until interrupted. The
repo comes from `-R`, `GH_REPO` or the git remote; `--pr N` narrows to one PR,
`--event` to a list of types, and `--since SEQ` replays buffered events after
that sequence number first. There is no `gh` fallback. Exit codes: 0 on
interrupt, 1 when the daemon is unavailable or the stream ended (stderr prints
the `--since` to reconnect with), 2 on bad usage.

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

## Agent skill

[`skills/forge-cache/SKILL.md`](skills/forge-cache/SKILL.md) teaches agents to
read PR state through `forge-cache pr` and to wait with `forge-cache watch`
instead of polling. The flake installs it two ways:

- `nix build .#skill` puts `SKILL.md` at the root of the output.
- `nix build .#forge-cache` includes it at
  `share/forge-cache/skills/forge-cache/SKILL.md`.

## Go client

```go
cl := client.New("") // "" = $FORGE_CACHE_SOCKET or the default socket
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
- Writes and mutations, PR details, threads and files, remote daemons, other
  forges.

## Development

```fish
nix develop                                   # dev shell with Go and tools
nix develop -c go test -race ./...
nix fmt                                       # treefmt
nix build -L .#checks.x86_64-linux.go-gate    # the CI gate
```
