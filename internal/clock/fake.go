package clock

import (
	"sync"
	"time"
)

// Fake is a manually advanced clock. Callbacks run synchronously inside Advance.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    int
}

type fakeTimer struct {
	f     *Fake
	at    time.Time
	seq   int
	fn    func()
	armed bool
}

func NewFake(t0 time.Time) *Fake { return &Fake{now: t0} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) AfterFunc(d time.Duration, fn func()) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	t := &fakeTimer{f: f, at: f.now.Add(d), seq: f.seq, fn: fn, armed: true}
	f.timers = append(f.timers, t)
	return t
}

// Pending returns the number of armed timers.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// Advance moves time forward by d, firing due timers in deadline order. Each
// callback sees Now equal to its deadline, and may arm further timers that
// fall within the window.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	for {
		next := f.nextDue(target)
		if next == nil {
			break
		}
		f.remove(next)
		if next.at.After(f.now) {
			f.now = next.at
		}
		f.mu.Unlock()
		next.fn()
		f.mu.Lock()
	}
	f.now = target
	f.mu.Unlock()
}

// nextDue returns the earliest armed timer due at or before target. Caller holds mu.
func (f *Fake) nextDue(target time.Time) *fakeTimer {
	var best *fakeTimer
	for _, t := range f.timers {
		if t.at.After(target) {
			continue
		}
		if best == nil || t.at.Before(best.at) || (t.at.Equal(best.at) && t.seq < best.seq) {
			best = t
		}
	}
	return best
}

// remove disarms t. Caller holds mu.
func (f *Fake) remove(t *fakeTimer) {
	t.armed = false
	for i, x := range f.timers {
		if x == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return
		}
	}
}

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if !t.armed {
		return false
	}
	t.f.remove(t)
	return true
}
