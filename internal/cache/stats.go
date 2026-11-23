package cache

type Stats struct {
	BatchPending int // change refs queued and not yet sent upstream
}

func (c *Cache) Stats() Stats {
	var s Stats
	for _, b := range c.batchers {
		s.BatchPending += b.pendingRefs()
	}
	return s
}
