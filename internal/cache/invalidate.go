package cache

import (
	"context"
	"time"

	"github.com/noamsto/forged/forge"
)

// Invalidate refetches k now, even when fresh, and returns once the result is
// stored. A change is also marked hot for HotHold; invalidating a repo-open
// list marks every cached entry of that repo stale.
func (c *Cache) Invalidate(ctx context.Context, k forge.Key) error {
	name, err := c.provider(k)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if rl := c.pause(name); rl != nil {
		c.mu.Unlock()
		return rl
	}
	now := c.o.Clock.Now()
	c.touch(k, false)
	switch {
	case !k.IsList():
		c.changeEntry(k).hotUntil = now.Add(c.o.HotHold)
	case k.Kind == forge.KindRepoOpen:
		for ck, e := range c.changes {
			if ck.Repo == k.Repo {
				e.fetchedAt = time.Time{}
			}
		}
	}
	c.mu.Unlock()

	return c.flight(ctx, "invalidate:"+k.String(), c.fetchTimeout(k), func(fctx context.Context) error {
		if k.IsList() {
			return c.fetchList(fctx, name, k)
		}
		return c.fetchChanges(name, []forge.ChangeRef{{Repo: k.Repo, Number: k.Number}})
	})
}
