package cache

type Stats struct {
	Keys         int // change and list entries
	Hot          int
	Warm         int
	Cold         int
	BatchPending int // change refs queued and not yet sent upstream
}

func (c *Cache) Stats() Stats {
	var s Stats
	c.mu.Lock()
	now := c.o.Clock.Now()
	members := c.leasedMembers(now)
	s.Keys = len(c.changes) + len(c.lists)
	for k, e := range c.changes {
		switch c.changeTier(now, k, e, members) {
		case tierHot:
			s.Hot++
		case tierWarm:
			s.Warm++
		default:
			s.Cold++
		}
	}
	for _, e := range c.lists {
		if c.leased(now, e.lastRead) {
			s.Warm++
		} else {
			s.Cold++
		}
	}
	c.mu.Unlock()
	for _, b := range c.batchers {
		s.BatchPending += b.pendingRefs()
	}
	return s
}
