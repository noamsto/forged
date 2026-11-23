package cache

import (
	"maps"
	"sync"
	"time"
)

type Stats struct {
	Keys         int // change and list entries
	Hot          int
	Warm         int
	Cold         int
	Inflight     int                  // upstream calls in progress
	BatchPending int                  // change refs queued and not yet sent upstream
	Calls        map[string]int       // upstream calls by "<provider key>.<Method>"
	PausedUntil  map[string]time.Time // rate-limited providers by key
}

type meter struct {
	mu       sync.Mutex
	inflight int
	calls    map[string]int
}

// begin records an upstream call and returns the function that ends it.
func (m *meter) begin(provider, method string) func() {
	m.mu.Lock()
	m.inflight++
	m.calls[provider+"."+method]++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		m.inflight--
		m.mu.Unlock()
	}
}

func (c *Cache) Stats() Stats {
	s := Stats{PausedUntil: map[string]time.Time{}}
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
	for name := range c.paused {
		if rl := c.pause(name); rl != nil {
			s.PausedUntil[name] = rl.RetryAfter
		}
	}
	c.mu.Unlock()

	c.meter.mu.Lock()
	s.Inflight = c.meter.inflight
	s.Calls = make(map[string]int, len(c.meter.calls))
	maps.Copy(s.Calls, c.meter.calls)
	c.meter.mu.Unlock()
	for _, b := range c.batchers {
		s.BatchPending += b.pendingRefs()
	}
	return s
}
