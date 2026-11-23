// Package cache is the freshness engine: max-age reads over forge providers
// with singleflight, list fill-through and (later) background polling.
package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/noamsto/forge-cache/forge"
)

type ReadOpts struct {
	MaxAge    time.Duration
	MaxAgeSet bool // false → Options.DefaultMaxAge; true with MaxAge 0 → fetch now
	Focus     bool
}

type Result struct {
	Changes   []forge.Change
	FetchedAt time.Time
	Negative  bool
}

type changeEntry struct {
	val        forge.Change
	ok         bool // false with a non-zero fetchedAt = negative (absent upstream)
	fetchedAt  time.Time
	fullAt     time.Time // last real fetch; a probe revalidation bumps only fetchedAt
	lastRead   time.Time
	loadedAt   time.Time // snapshot load time: delays eviction without leasing
	hotUntil   time.Time
	focusUntil time.Time
	poll
	lastErr error
}

// negative reports a definitive absence; a never-fetched entry is not negative.
func (e *changeEntry) negative() bool { return !e.ok && !e.fetchedAt.IsZero() }

type listEntry struct {
	refs          []forge.ChangeRef
	listFetchedAt time.Time
	fullAt        time.Time
	lastRead      time.Time
	loadedAt      time.Time
	poll
}

type Cache struct {
	o         Options
	providers map[string]forge.Provider
	batchers  map[string]*batcher
	flights   singleflight.Group

	mu           sync.Mutex
	changes      map[forge.Key]*changeEntry
	lists        map[forge.Key]*listEntry
	inflightList map[forge.RepoRef]chan struct{}  // closed when that repo-open fetch has stored
	paused       map[string]*forge.RateLimitError // by provider name, until RetryAfter
	subs         map[*subscriber]struct{}
	meter        meter
}

// New builds a Cache over providers keyed by forge+"/"+host (e.g. "github/github.com").
func New(providers map[string]forge.Provider, o Options) *Cache {
	c := &Cache{
		o:            o,
		providers:    providers,
		batchers:     make(map[string]*batcher, len(providers)),
		changes:      map[forge.Key]*changeEntry{},
		lists:        map[forge.Key]*listEntry{},
		inflightList: map[forge.RepoRef]chan struct{}{},
		paused:       map[string]*forge.RateLimitError{},
		subs:         map[*subscriber]struct{}{},
		meter:        meter{calls: map[string]int{}},
	}
	for name, p := range providers {
		c.batchers[name] = newBatcher(name, p, o.Clock, o.BatchWindow, &c.meter)
	}
	return c
}

// Read returns k's value, fetching it when the cached one is older than the
// effective max-age. An absent change key returns forge.ErrNotFound.
func (c *Cache) Read(ctx context.Context, k forge.Key, o ReadOpts) (Result, error) {
	name, err := c.provider(k)
	if err != nil {
		return Result{}, err
	}
	maxAge := c.o.DefaultMaxAge
	if o.MaxAgeSet {
		maxAge = o.MaxAge
	}

	c.mu.Lock()
	c.touch(k, o.Focus)
	fresh := c.fresh(k, maxAge)
	var listDone chan struct{}
	if !fresh && !k.IsList() && maxAge > 0 {
		listDone = c.inflightList[k.Repo]
	}
	var staleMembers []forge.ChangeRef
	if !fresh && k.IsList() {
		staleMembers = c.staleMembers(k, maxAge)
	}
	c.mu.Unlock()

	if listDone != nil {
		select {
		case <-listDone:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		c.mu.Lock()
		fresh = c.fresh(k, maxAge)
		c.mu.Unlock()
	}
	if !fresh {
		c.mu.Lock()
		rl := c.pause(name)
		c.mu.Unlock()
		if rl != nil {
			return Result{}, rl
		}
	}

	switch {
	case fresh:
	case len(staleMembers) > 0:
		err = c.flight(ctx, "members:"+k.String(), func(context.Context) error {
			return c.fetchChanges(name, staleMembers)
		})
	default:
		err = c.flight(ctx, k.String(), func(fctx context.Context) error {
			if k.IsList() {
				return c.fetchList(fctx, name, k)
			}
			return c.fetchChanges(name, []forge.ChangeRef{{Repo: k.Repo, Number: k.Number}})
		})
	}
	if err != nil {
		return Result{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result(k)
}

// provider returns the name of the provider serving k.
func (c *Cache) provider(k forge.Key) (string, error) {
	if k.Kind == forge.KindQuery {
		// Query keys name only the forge; pick its lowest-sorting host for determinism.
		names := make([]string, 0, len(c.providers))
		for name := range c.providers {
			if strings.HasPrefix(name, k.Repo.Forge+"/") {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			return names[0], nil
		}
	} else if name := k.Repo.Forge + "/" + k.Repo.Host; c.providers[name] != nil {
		return name, nil
	}
	return "", fmt.Errorf("%s: %w", k, forge.ErrUnsupported)
}

// touch creates k's entry if needed and renews its lease. Requires c.mu.
func (c *Cache) touch(k forge.Key, focus bool) {
	now := c.o.Clock.Now()
	if k.IsList() {
		e := c.lists[k]
		if e == nil {
			e = &listEntry{}
			c.lists[k] = e
		}
		e.lastRead = now
		return
	}
	e := c.changeEntry(k)
	e.lastRead = now
	if focus {
		e.focusUntil = now.Add(c.o.LeaseTTL)
	}
}

// changeEntry returns k's entry, creating an unfetched one. Requires c.mu.
func (c *Cache) changeEntry(k forge.Key) *changeEntry {
	e := c.changes[k]
	if e == nil {
		e = &changeEntry{}
		c.changes[k] = e
	}
	return e
}

// fresh reports whether k can be served from memory. Requires c.mu.
func (c *Cache) fresh(k forge.Key, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	now := c.o.Clock.Now()
	if k.IsList() {
		e := c.lists[k]
		fetchedAt := c.listFetchedAt(e)
		if fetchedAt.IsZero() {
			return false
		}
		if c.listNegative(k, e) {
			maxAge = min(maxAge, c.o.NegativeTTL)
		}
		return now.Sub(fetchedAt) < maxAge
	}
	e := c.changes[k]
	if e.fetchedAt.IsZero() {
		return false
	}
	switch {
	case e.negative():
		maxAge = min(maxAge, c.o.NegativeTTL)
	case e.val.IsTerminal():
		maxAge = max(maxAge, c.o.TerminalTTL)
	}
	return now.Sub(e.fetchedAt) < maxAge
}

// listFetchedAt is the oldest of the list's own and its members' fetch times;
// zero when any part was never fetched or was marked stale. Requires c.mu.
func (c *Cache) listFetchedAt(e *listEntry) time.Time {
	oldest := e.listFetchedAt
	for _, ref := range e.refs {
		m := c.changes[forge.ChangeKey(ref.Repo, ref.Number)]
		if m == nil || m.fetchedAt.IsZero() {
			return time.Time{}
		}
		if m.fetchedAt.Before(oldest) {
			oldest = m.fetchedAt
		}
	}
	return oldest
}

// listNegative: only an empty change-by-branch answer is negative. Requires c.mu.
func (c *Cache) listNegative(k forge.Key, e *listEntry) bool {
	return k.Kind == forge.KindByBranch && len(e.refs) == 0 && !e.listFetchedAt.IsZero()
}

// staleMembers returns the members of list k that need a refetch when the
// list part itself is still fresh; nil when the whole list must be fetched.
// Requires c.mu.
func (c *Cache) staleMembers(k forge.Key, maxAge time.Duration) []forge.ChangeRef {
	e := c.lists[k]
	if maxAge <= 0 || e.listFetchedAt.IsZero() || c.o.Clock.Now().Sub(e.listFetchedAt) >= maxAge {
		return nil
	}
	var stale []forge.ChangeRef
	for _, ref := range e.refs {
		mk := forge.ChangeKey(ref.Repo, ref.Number)
		if c.changes[mk] == nil || !c.fresh(mk, maxAge) {
			stale = append(stale, ref)
		}
	}
	return stale
}

// flight runs fn once per key across concurrent callers. The shared fetch
// runs detached from any one reader's ctx so a cancelling reader cannot fail
// the others that joined it.
func (c *Cache) flight(ctx context.Context, key string, fn func(context.Context) error) error {
	ch := c.flights.DoChan(key, func() (any, error) {
		return nil, fn(context.WithoutCancel(ctx))
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetchChanges fetches refs through the provider's batcher and applies each
// outcome. It returns the first per-ref error other than absence.
func (c *Cache) fetchChanges(name string, refs []forge.ChangeRef) error {
	got := c.batchers[name].do(refs, false)

	c.mu.Lock()
	defer c.mu.Unlock()
	failed := c.applyFetched(name, got)
	for _, ref := range refs {
		if err := failed[ref]; err != nil {
			return err
		}
	}
	return nil
}

// applyFetched records each outcome and returns the errors of the refs that
// failed for a reason other than absence. Requires c.mu.
func (c *Cache) applyFetched(name string, got map[forge.ChangeRef]forge.Fetched) map[forge.ChangeRef]error {
	now := c.o.Clock.Now()
	failed := map[forge.ChangeRef]error{}
	for ref, f := range got {
		switch {
		case f.Err == nil:
			c.store(now, []forge.Change{f.Change})
		case errors.Is(f.Err, forge.ErrNotFound):
			c.storeNegative(now, ref)
		default:
			if e := c.changes[forge.ChangeKey(ref.Repo, ref.Number)]; e != nil {
				e.lastErr = f.Err
			}
			c.notePause(name, f.Err)
			failed[ref] = f.Err
		}
	}
	return failed
}

func (c *Cache) fetchList(ctx context.Context, name string, k forge.Key) error {
	p := c.providers[name]
	if k.Kind == forge.KindRepoOpen {
		done := make(chan struct{})
		c.mu.Lock()
		c.inflightList[k.Repo] = done
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			delete(c.inflightList, k.Repo)
			c.mu.Unlock()
			close(done)
		}()
	}
	var changes []forge.Change
	var err error
	switch k.Kind {
	case forge.KindRepoOpen:
		end := c.meter.begin(name, "ListOpen")
		changes, err = p.ListOpen(ctx, k.Repo)
		end()
	case forge.KindByBranch:
		end := c.meter.begin(name, "ByBranch")
		changes, err = p.ByBranch(ctx, k.Repo, k.Branch)
		end()
	default: // forge.KindQuery
		end := c.meter.begin(name, "Search")
		changes, err = p.Search(ctx, k.Query, k.Limit)
		end()
	}
	if err != nil {
		c.mu.Lock()
		c.notePause(name, err)
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Clock.Now()
	c.store(now, changes)
	refs := make([]forge.ChangeRef, len(changes))
	for i, ch := range changes {
		refs[i] = forge.ChangeRef{Repo: ch.Repo, Number: ch.Number}
	}
	if k.Kind == forge.KindRepoOpen {
		c.staleDropped(k.Repo, refs)
	}
	e := c.lists[k]
	if e == nil {
		e = &listEntry{}
		c.lists[k] = e
	}
	e.refs = refs
	e.listFetchedAt, e.fullAt = now, now
	e.backoff = 0
	return nil
}

// staleDropped zeroes fetchedAt of cached open changes of r that a fresh
// repo-open result no longer holds, so their next read refetches. Requires c.mu.
func (c *Cache) staleDropped(r forge.RepoRef, open []forge.ChangeRef) {
	for k, e := range c.changes {
		if k.Repo != r || !e.ok || !e.val.IsOpen() {
			continue
		}
		if !slices.Contains(open, forge.ChangeRef{Repo: r, Number: k.Number}) {
			e.fetchedAt = time.Time{}
		}
	}
}

// store writes fetched changes through to their change entries; every fetch
// path applies its results here. Requires c.mu.
func (c *Cache) store(now time.Time, changes []forge.Change) {
	for _, ch := range changes {
		e := c.changeEntry(forge.ChangeKey(ch.Repo, ch.Number))
		old := e.val
		e.val, e.ok, e.fetchedAt, e.fullAt = ch, true, now, now
		e.lastErr, e.backoff = nil, 0
		c.publish(old, ch)
	}
}

// storeNegative records a definitive absence. Requires c.mu.
func (c *Cache) storeNegative(now time.Time, ref forge.ChangeRef) {
	e := c.changeEntry(forge.ChangeKey(ref.Repo, ref.Number))
	e.val, e.ok, e.fetchedAt, e.fullAt = forge.Change{}, false, now, now
	e.lastErr, e.backoff = nil, 0
}

// result builds k's Result from memory. Requires c.mu.
func (c *Cache) result(k forge.Key) (Result, error) {
	if !k.IsList() {
		e := c.changes[k]
		if !e.ok {
			return Result{FetchedAt: e.fetchedAt, Negative: true}, fmt.Errorf("%s: %w", k, forge.ErrNotFound)
		}
		return Result{Changes: []forge.Change{e.val}, FetchedAt: e.fetchedAt}, nil
	}
	e := c.lists[k]
	res := Result{FetchedAt: c.listFetchedAt(e), Negative: c.listNegative(k, e)}
	for _, ref := range e.refs {
		if m := c.changes[forge.ChangeKey(ref.Repo, ref.Number)]; m != nil && m.ok {
			res.Changes = append(res.Changes, m.val)
		}
	}
	return res, nil
}
