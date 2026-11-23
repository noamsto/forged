package cache

import (
	"slices"

	"github.com/noamsto/forged/forge"
)

// liveBuffer is the room a subscriber channel has for live events beyond its replay.
const liveBuffer = 256

// Filter selects the events a subscriber receives. Number 0 means the whole
// repo, empty Types every type; with HasSince, buffered events after Since are
// replayed first.
type Filter struct {
	Repo     forge.RepoRef
	Number   int
	Types    []forge.EventType
	Since    uint64
	HasSince bool
}

// matches reports whether k is kept leased by the subscription: its change
// keys and, for a whole-repo one, the repo's open list.
func (f Filter) matches(k forge.Key) bool {
	if k.Repo != f.Repo {
		return false
	}
	switch k.Kind {
	case forge.KindChange:
		return f.Number == 0 || f.Number == k.Number
	case forge.KindRepoOpen:
		return f.Number == 0
	default:
		return false
	}
}

func (f Filter) matchesEvent(ev forge.ChangeEvent) bool {
	return ev.Repo == f.Repo && (f.Number == 0 || f.Number == ev.Number) &&
		(len(f.Types) == 0 || slices.Contains(f.Types, ev.Type))
}

func (f Filter) key() forge.Key {
	if f.Number > 0 {
		return forge.ChangeKey(f.Repo, f.Number)
	}
	return forge.RepoOpen(f.Repo)
}

type subscriber struct {
	f  Filter
	ch chan forge.ChangeEvent
}

// Subscribe streams the typed events of the changes f selects and keeps them
// leased and hot-polled while subscribed. A subscriber that falls behind has
// its channel closed and should resubscribe with its last seq as Since; cancel
// stops delivery and closes the channel.
func (c *Cache) Subscribe(f Filter) (<-chan forge.ChangeEvent, func(), error) {
	k := f.key()
	if _, err := c.provider(k); err != nil {
		return nil, nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if k.IsList() {
		c.listEntry(k)
	} else {
		c.changeEntry(k)
	}
	var replay []forge.ChangeEvent
	if f.HasSince {
		if oldest := c.events.oldestSeq(); f.Since < oldest-1 {
			replay = append(replay, forge.ChangeEvent{Type: forge.EventResync, Seq: oldest - 1})
		}
		replay = append(replay, c.events.since(f.Since, f)...)
	}
	s := &subscriber{f: f, ch: make(chan forge.ChangeEvent, len(replay)+liveBuffer)}
	for _, ev := range replay {
		s.ch <- ev
	}
	c.subs[s] = struct{}{}
	return s.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[s]; ok {
			delete(c.subs, s)
			close(s.ch)
		}
	}, nil
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
