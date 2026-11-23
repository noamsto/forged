package cache

import (
	"time"

	"github.com/noamsto/forged/forge"
)

// TestSetFetchedAt overrides the fetch time of a change entry or, for a list
// key, of the list itself.
func (c *Cache) TestSetFetchedAt(k forge.Key, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k.IsList() {
		c.listEntry(k).listFetchedAt = t
		return
	}
	c.changeEntry(k).fetchedAt = t
}

func (c *Cache) TestBatchPending() int { return c.Stats().BatchPending }
