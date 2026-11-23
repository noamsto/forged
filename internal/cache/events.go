package cache

import (
	"time"

	"github.com/noamsto/forged/forge"
)

// eventLog numbers events and keeps the last Options.EventBuffer of them for replay.
type eventLog struct {
	ring []forge.ChangeEvent // once full, ring[head] is the oldest
	head int
	seq  uint64 // last assigned
}

// newEventLog seeds seq from the wall clock so a restarted daemon numbers its
// events above any cursor an earlier run handed out.
func newEventLog(now time.Time, size int) eventLog {
	return eventLog{ring: make([]forge.ChangeEvent, 0, size), seq: uint64(now.UnixMicro())}
}

func (l *eventLog) add(ev forge.ChangeEvent) {
	if len(l.ring) < cap(l.ring) {
		l.ring = append(l.ring, ev)
		return
	}
	l.ring[l.head] = ev
	l.head = (l.head + 1) % len(l.ring)
}

// oldestSeq is the seq of the oldest buffered event, or the next seq when none is.
func (l *eventLog) oldestSeq() uint64 {
	if len(l.ring) == 0 {
		return l.seq + 1
	}
	return l.ring[l.head].Seq
}

// since returns the buffered events with Seq > after that match f, oldest first.
func (l *eventLog) since(after uint64, f Filter) []forge.ChangeEvent {
	var evs []forge.ChangeEvent
	for i := range l.ring {
		ev := l.ring[(l.head+i)%len(l.ring)]
		if ev.Seq > after && f.matchesEvent(ev) {
			evs = append(evs, ev)
		}
	}
	return evs
}

// emit stamps, buffers and fans out evs. A subscriber whose channel is full is
// closed and dropped rather than blocking the cache or losing an event
// silently; it reconnects from its last seq. Requires c.mu.
func (c *Cache) emit(now time.Time, evs []forge.ChangeEvent) {
	for _, ev := range evs {
		c.events.seq++
		ev.Seq, ev.At = c.events.seq, now
		c.events.add(ev)
		for s := range c.subs {
			if !s.f.matchesEvent(ev) {
				continue
			}
			select {
			case s.ch <- ev:
			default:
				delete(c.subs, s)
				close(s.ch)
			}
		}
	}
}
