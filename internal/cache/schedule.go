package cache

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

// poll is an entry's scheduler backoff state.
type poll struct {
	nextPoll time.Time
	backoff  time.Duration
}

// due reports whether an entry last fetched at last is due its every-interval
// poll; a backed-off entry waits for nextPoll as well.
func (p *poll) due(now, last time.Time, interval time.Duration) bool {
	if p.backoff > 0 && now.Before(p.nextPoll) {
		return false
	}
	return now.Sub(last) >= interval
}

// fail doubles the poll interval, starting from interval, up to maxBackoff.
func (p *poll) fail(now time.Time, interval, maxBackoff time.Duration) {
	p.backoff = min(max(2*p.backoff, 2*interval), maxBackoff)
	p.nextPoll = now.Add(p.backoff)
}

type tier int

const (
	tierCold tier = iota
	tierWarm
	tierHot
)

func (c *Cache) leased(now, lastRead time.Time) bool { return now.Sub(lastRead) < c.o.LeaseTTL }

// leasedMembers returns the members of every leased list; they count as
// leased themselves. Requires c.mu.
func (c *Cache) leasedMembers(now time.Time) map[forge.ChangeRef]bool {
	members := map[forge.ChangeRef]bool{}
	for _, e := range c.lists {
		if c.leased(now, e.lastRead) {
			for _, ref := range e.refs {
				members[ref] = true
			}
		}
	}
	return members
}

// hotContent: state the repo probe cannot see moving, so it is polled directly.
func (c *Cache) hotContent(now time.Time, e *changeEntry) bool {
	return e.ok && e.val.IsOpen() && (e.val.Checks.Rollup == forge.RollupPending ||
		e.val.Mergeable == forge.MergeableUnknown ||
		now.Before(e.focusUntil) || now.Before(e.hotUntil))
}

// changeTier requires c.mu.
func (c *Cache) changeTier(now time.Time, k forge.Key, e *changeEntry, members map[forge.ChangeRef]bool) tier {
	switch {
	case !c.leased(now, e.lastRead) && !members[forge.ChangeRef{Repo: k.Repo, Number: k.Number}]:
		return tierCold
	case c.hotContent(now, e):
		return tierHot
	}
	return tierWarm
}

// tickPlan is one provider's due work for a tick.
type tickPlan struct {
	changes map[forge.ChangeRef]time.Duration // polled directly; value = poll interval
	lists   []forge.Key                       // refetched directly
	repos   map[forge.RepoRef]*repoPlan
}

// repoPlan is one repo's probe-covered warm set; probed once when any part is due.
type repoPlan struct {
	due     bool
	lists   []forge.Key // leased repo-open and change-by-branch keys
	changes []forge.ChangeRef
}

func (tp *tickPlan) repo(r forge.RepoRef) *repoPlan {
	rp := tp.repos[r]
	if rp == nil {
		rp = &repoPlan{}
		tp.repos[r] = rp
	}
	return rp
}

// Tick runs one scheduler pass: it evicts long-unread entries, then polls the
// due leased entries of every unpaused provider. Upstream calls run outside c.mu.
func (c *Cache) Tick(ctx context.Context) {
	c.mu.Lock()
	now := c.o.Clock.Now()
	c.evict(now)
	plans := c.plan(now)
	c.mu.Unlock()

	for _, name := range slices.Sorted(maps.Keys(plans)) {
		c.runPlan(ctx, name, plans[name])
	}
}

// evict drops entries unread for EvictAfter; a change stays while a kept list
// holds it. Requires c.mu.
func (c *Cache) evict(now time.Time) {
	for k, e := range c.lists {
		if now.Sub(e.lastRead) >= c.o.EvictAfter {
			delete(c.lists, k)
		}
	}
	kept := map[forge.ChangeRef]bool{}
	for _, e := range c.lists {
		for _, ref := range e.refs {
			kept[ref] = true
		}
	}
	for k, e := range c.changes {
		if now.Sub(e.lastRead) >= c.o.EvictAfter && !kept[forge.ChangeRef{Repo: k.Repo, Number: k.Number}] {
			delete(c.changes, k)
		}
	}
}

// plan collects the due work per provider. Requires c.mu.
func (c *Cache) plan(now time.Time) map[string]*tickPlan {
	plans := map[string]*tickPlan{}
	planFor := func(k forge.Key) *tickPlan {
		name, _ := c.provider(k) // every cached key passed provider() on its first read
		if c.pause(name) != nil {
			return nil
		}
		tp := plans[name]
		if tp == nil {
			tp = &tickPlan{changes: map[forge.ChangeRef]time.Duration{}, repos: map[forge.RepoRef]*repoPlan{}}
			plans[name] = tp
		}
		return tp
	}

	// Members of a list refetched this tick come back with it.
	refetched := map[forge.ChangeRef]bool{}
	for k, e := range c.lists {
		if !c.leased(now, e.lastRead) {
			continue
		}
		tp := planFor(k)
		if tp == nil {
			continue
		}
		switch {
		case k.Kind == forge.KindQuery:
			if !e.due(now, e.listFetchedAt, c.o.WarmInterval) {
				continue
			}
		case !e.due(now, e.fullAt, c.o.WarmMaxAge):
			rp := tp.repo(k.Repo)
			rp.lists = append(rp.lists, k)
			rp.due = rp.due || e.due(now, e.listFetchedAt, c.o.WarmInterval)
			continue
		}
		tp.lists = append(tp.lists, k)
		for _, ref := range e.refs {
			refetched[ref] = true
		}
	}

	members := c.leasedMembers(now)
	for k, e := range c.changes {
		ref := forge.ChangeRef{Repo: k.Repo, Number: k.Number}
		t := c.changeTier(now, k, e, members)
		if t == tierCold || refetched[ref] {
			continue
		}
		tp := planFor(k)
		if tp == nil {
			continue
		}
		fullAt := e.fullAt
		if e.fetchedAt.IsZero() { // marked stale: refetch rather than revalidate
			fullAt = time.Time{}
		}
		switch {
		case t == tierHot:
			if e.due(now, e.fetchedAt, c.o.HotInterval) {
				tp.changes[ref] = c.o.HotInterval
			}
		case e.ok && e.val.IsTerminal():
			if e.due(now, e.fetchedAt, c.o.TerminalTTL) {
				tp.changes[ref] = c.o.TerminalTTL
			}
		case e.due(now, fullAt, c.o.WarmMaxAge):
			tp.changes[ref] = c.o.WarmMaxAge
		default:
			rp := tp.repo(k.Repo)
			rp.changes = append(rp.changes, ref)
			rp.due = rp.due || e.due(now, e.fetchedAt, c.o.WarmInterval)
		}
	}
	return plans
}

func (c *Cache) runPlan(ctx context.Context, name string, tp *tickPlan) {
	if len(tp.changes) > 0 && !c.isPaused(name) {
		c.pollChanges(name, tp.changes)
	}
	for _, k := range tp.lists {
		c.pollList(ctx, name, k)
	}
	for _, r := range slices.SortedFunc(maps.Keys(tp.repos), func(a, b forge.RepoRef) int {
		return cmp.Compare(a.String(), b.String())
	}) {
		if rp := tp.repos[r]; rp.due {
			c.probe(ctx, name, r, rp)
		}
	}
}

// pollChanges fetches refs in one immediate batch; a failed ref backs off from
// its poll interval.
func (c *Cache) pollChanges(name string, refs map[forge.ChangeRef]time.Duration) {
	got := c.batchers[name].do(slices.Collect(maps.Keys(refs)), true)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Clock.Now()
	for ref := range c.applyFetched(name, got) {
		c.changes[forge.ChangeKey(ref.Repo, ref.Number)].fail(now, refs[ref], c.o.MaxBackoff)
	}
}

// pollList refetches list k, joining a reader's in-flight fetch of it.
func (c *Cache) pollList(ctx context.Context, name string, k forge.Key) {
	if c.isPaused(name) {
		return
	}
	err := c.flight(ctx, k.String(), func(fctx context.Context) error {
		return c.fetchList(fctx, name, k)
	})
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists[k].fail(c.o.Clock.Now(), c.o.WarmInterval, c.o.MaxBackoff)
}

// probe asks whether r changed: changed refetches its warm set, unchanged
// revalidates it (hot changes excepted, as the probe cannot see their state).
func (c *Cache) probe(ctx context.Context, name string, r forge.RepoRef, rp *repoPlan) {
	if c.isPaused(name) {
		return
	}
	res, err := c.providers[name].Probe(ctx, r)

	c.mu.Lock()
	now := c.o.Clock.Now()
	switch {
	case err != nil:
		c.notePause(name, err)
		for _, k := range rp.lists {
			c.lists[k].fail(now, c.o.WarmInterval, c.o.MaxBackoff)
		}
		for _, ref := range rp.changes {
			c.changes[forge.ChangeKey(ref.Repo, ref.Number)].fail(now, c.o.WarmInterval, c.o.MaxBackoff)
		}
		c.mu.Unlock()
		return
	case !res.Changed:
		revalidate := slices.Clone(rp.changes)
		for _, k := range rp.lists {
			e := c.lists[k]
			e.listFetchedAt, e.backoff = now, 0
			revalidate = append(revalidate, e.refs...)
		}
		for _, ref := range revalidate {
			e := c.changes[forge.ChangeKey(ref.Repo, ref.Number)]
			if e.fetchedAt.IsZero() || c.hotContent(now, e) {
				continue
			}
			e.fetchedAt, e.backoff = now, 0
		}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	for _, k := range rp.lists {
		c.pollList(ctx, name, k)
	}
	c.mu.Lock()
	inLists := map[forge.ChangeRef]bool{}
	for _, k := range rp.lists {
		for _, ref := range c.lists[k].refs {
			inLists[ref] = true
		}
	}
	refs := map[forge.ChangeRef]time.Duration{}
	for _, ref := range rp.changes {
		if !inLists[ref] {
			refs[ref] = c.o.WarmInterval
		}
	}
	c.mu.Unlock()
	if len(refs) > 0 && !c.isPaused(name) {
		c.pollChanges(name, refs)
	}
}

// Run ticks every Options.Tick until ctx is done.
func (c *Cache) Run(ctx context.Context) {
	var arm func()
	arm = func() {
		c.o.Clock.AfterFunc(c.o.Tick, func() {
			if ctx.Err() != nil {
				return
			}
			c.Tick(ctx)
			arm()
		})
	}
	arm()
	<-ctx.Done()
}

// pause returns the rate limit pausing provider name, clearing a lapsed one.
// Requires c.mu.
func (c *Cache) pause(name string) *forge.RateLimitError {
	rl := c.paused[name]
	if rl != nil && !c.o.Clock.Now().Before(rl.RetryAfter) {
		delete(c.paused, name)
		return nil
	}
	return rl
}

func (c *Cache) isPaused(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pause(name) != nil
}

// notePause pauses provider name when err is a rate limit. Requires c.mu.
func (c *Cache) notePause(name string, err error) {
	var rl *forge.RateLimitError
	if !errors.As(err, &rl) {
		return
	}
	if cur := c.paused[name]; cur == nil || rl.RetryAfter.After(cur.RetryAfter) {
		c.paused[name] = rl
	}
}
