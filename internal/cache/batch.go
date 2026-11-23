package cache

import (
	"context"
	"sync"
	"time"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/clock"
)

const defaultMaxBatch = 50

// batcher coalesces change fetches for one provider into GetMany calls: refs
// queue for the batch window or until MaxBatch accumulate.
type batcher struct {
	p      forge.Provider
	clock  clock.Clock
	window time.Duration
	max    int

	mu      sync.Mutex
	pending map[forge.ChangeRef][]chan forge.Fetched
	timer   clock.Timer
	gen     int // invalidates the callback of a timer whose refs were already taken
}

func newBatcher(p forge.Provider, clk clock.Clock, window time.Duration) *batcher {
	maxBatch := p.Capabilities().MaxBatch
	if maxBatch == 0 {
		maxBatch = defaultMaxBatch
	}
	return &batcher{
		p:       p,
		clock:   clk,
		window:  window,
		max:     maxBatch,
		pending: map[forge.ChangeRef][]chan forge.Fetched{},
	}
}

// do queues refs and returns each one's outcome once its batch was fetched.
func (b *batcher) do(refs []forge.ChangeRef) map[forge.ChangeRef]forge.Fetched {
	waits := make(map[forge.ChangeRef]chan forge.Fetched, len(refs))
	b.mu.Lock()
	for _, ref := range refs {
		ch := make(chan forge.Fetched, 1)
		waits[ref] = ch
		b.pending[ref] = append(b.pending[ref], ch)
	}
	var batches []batch
	if b.window == 0 {
		batches = b.take(1)
	} else {
		batches = b.take(b.max)
		if len(b.pending) > 0 && b.timer == nil {
			gen := b.gen
			b.timer = b.clock.AfterFunc(b.window, func() { go b.flush(gen) })
		}
	}
	b.mu.Unlock()

	for _, batch := range batches {
		b.run(batch)
	}
	out := make(map[forge.ChangeRef]forge.Fetched, len(waits))
	for ref, ch := range waits {
		out[ref] = <-ch
	}
	return out
}

// flush fetches everything queued when the window timer of generation gen fires.
func (b *batcher) flush(gen int) {
	b.mu.Lock()
	if gen != b.gen {
		b.mu.Unlock()
		return
	}
	b.timer = nil
	b.gen++
	batches := b.take(1)
	b.mu.Unlock()

	for _, batch := range batches {
		b.run(batch)
	}
}

type batch map[forge.ChangeRef][]chan forge.Fetched

// take removes queued refs with their waiters in chunks of at most b.max while
// at least atLeast remain, and retires the window timer once the queue is
// empty. Requires b.mu.
func (b *batcher) take(atLeast int) []batch {
	var batches []batch
	for len(b.pending) > 0 && len(b.pending) >= atLeast {
		bt := make(batch, min(b.max, len(b.pending)))
		for ref, waiters := range b.pending {
			if len(bt) == b.max {
				break
			}
			bt[ref] = waiters
			delete(b.pending, ref)
		}
		batches = append(batches, bt)
	}
	if len(b.pending) == 0 && b.timer != nil {
		b.timer.Stop()
		b.timer = nil
		b.gen++
	}
	return batches
}

// run issues one GetMany for bt and answers every waiter; a failed call is
// that ref's outcome for all of them.
func (b *batcher) run(bt batch) {
	refs := make([]forge.ChangeRef, 0, len(bt))
	for ref := range bt {
		refs = append(refs, ref)
	}
	// Detached: waiters from several readers share this call.
	got, err := b.p.GetMany(context.Background(), refs)
	for ref, waiters := range bt {
		f := got[ref]
		if err != nil {
			f = forge.Fetched{Err: err}
		}
		for _, ch := range waiters {
			ch <- f
		}
	}
}

func (b *batcher) pendingRefs() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}
