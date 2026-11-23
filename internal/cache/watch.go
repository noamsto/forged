package cache

import (
	"reflect"

	"github.com/noamsto/forge-cache/forge"
)

const subscriberBuffer = 64

// Filter selects the changes a subscriber receives; Number 0 means the whole repo.
type Filter struct {
	Repo   forge.RepoRef
	Number int
}

func (f Filter) matches(k forge.Key) bool {
	return k.Kind == forge.KindChange && k.Repo == f.Repo && (f.Number == 0 || f.Number == k.Number)
}

type subscriber struct {
	f  Filter
	ch chan forge.Event
}

// Subscribe streams an event whenever a matching change's stored value
// changes. Matching keys stay leased while subscribed. A subscriber that falls
// 64 events behind loses its oldest ones; cancel stops delivery and closes the channel.
func (c *Cache) Subscribe(f Filter) (<-chan forge.Event, func()) {
	s := &subscriber{f: f, ch: make(chan forge.Event, subscriberBuffer)}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[s]; ok {
			delete(c.subs, s)
			close(s.ch)
		}
	}
}

// watched reports whether a subscriber matches k. Requires c.mu.
func (c *Cache) watched(k forge.Key) bool {
	for s := range c.subs {
		if s.f.matches(k) {
			return true
		}
	}
	return false
}

// publish emits an event when a change's stored value differs from the
// previous one. Requires c.mu.
func (c *Cache) publish(old, cur forge.Change) {
	if reflect.DeepEqual(old, cur) {
		return
	}
	k := forge.ChangeKey(cur.Repo, cur.Number)
	ev := forge.Event{Key: k, Change: cur}
	for s := range c.subs {
		if !s.f.matches(k) {
			continue
		}
		for {
			select {
			case s.ch <- ev:
			default:
				select { // full: drop the oldest; only publishers under c.mu send
				case <-s.ch:
				default:
				}
				continue
			}
			break
		}
	}
}
