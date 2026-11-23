package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
)

func recvEvent(t *testing.T, ch <-chan forge.Event) forge.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return forge.Event{}
	}
}

// wantNoEvent is only called after the synchronous call that would publish
// has returned, so an immediate poll is enough.
func wantNoEvent(t *testing.T, ch <-chan forge.Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	default:
	}
}

func TestWatchReceivesUpdatedChange(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	defer cancel()

	updated := withTitle(openChange(repoA, 1), "new title")
	fake.SetChanges(repoA, []forge.Change{updated})
	mustInvalidate(t, c, k)

	ev := recvEvent(t, ch)
	if ev.Key != k || ev.Change.Title != "new title" {
		t.Fatalf("event = %+v", ev)
	}
	wantNoEvent(t, ch)
}

func TestWatchIdenticalRefetchEmitsNothing(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	defer cancel()

	mustInvalidate(t, c, k)
	wantCalls(t, fake, "GetMany", 2)
	wantNoEvent(t, ch)
}

func TestWatchNumberFilter(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})
	k1, k2 := forge.ChangeKey(repoA, 1), forge.ChangeKey(repoA, 2)
	mustRead(t, c, k1, ReadOpts{})
	mustRead(t, c, k2, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA, Number: 2})
	defer cancel()

	fake.SetChanges(repoA, []forge.Change{
		withTitle(openChange(repoA, 1), "one"),
		withTitle(openChange(repoA, 2), "two"),
	})
	mustInvalidate(t, c, k1)
	wantNoEvent(t, ch)

	mustInvalidate(t, c, k2)
	ev := recvEvent(t, ch)
	if ev.Key != k2 || ev.Change.Title != "two" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchRepoFilterExcludesOtherRepos(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoB, []forge.Change{openChange(repoB, 1)})
	k := forge.ChangeKey(repoB, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	defer cancel()

	fake.SetChanges(repoB, []forge.Change{withTitle(openChange(repoB, 1), "other")})
	mustInvalidate(t, c, k)
	wantNoEvent(t, ch)
}

func TestWatchKeepsKeyLeasedPastLeaseTTL(t *testing.T) {
	c, clk, fake := newTestCache(t)
	pending := withRollup(openChange(repoA, 1), forge.RollupPending)
	fake.SetChanges(repoA, []forge.Change{pending})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	defer cancel()

	fake.SetChanges(repoA, []forge.Change{withRollup(pending, forge.RollupPass)})
	clk.Advance(DefaultOptions().LeaseTTL + time.Second)
	c.Tick(context.Background())

	wantCalls(t, fake, "GetMany", 2)
	ev := recvEvent(t, ch)
	if ev.Key != k || ev.Change.Checks.Rollup != forge.RollupPass {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchCancelStopsDelivery(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	cancel()

	fake.SetChanges(repoA, []forge.Change{withTitle(openChange(repoA, 1), "after cancel")})
	mustInvalidate(t, c, k)
	wantNoEvent(t, ch)
}

func TestWatchFullBufferDropsOldestWithoutBlocking(t *testing.T) {
	const (
		buffer = 64
		total  = 70
	)
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := c.Subscribe(Filter{Repo: repoA})
	defer cancel()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(done)
		for i := range total {
			fake.SetChanges(repoA, []forge.Change{withTitle(openChange(repoA, 1), fmt.Sprintf("t%d", i))})
			if err := c.Invalidate(context.Background(), k); err != nil {
				t.Errorf("Invalidate: %v", err)
				return
			}
		}
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fetches blocked on a slow subscriber")
	}
	wg.Wait()

	if got := len(ch); got != buffer {
		t.Fatalf("buffered events = %d, want %d", got, buffer)
	}
	first := recvEvent(t, ch)
	if want := fmt.Sprintf("t%d", total-buffer); first.Change.Title != want {
		t.Fatalf("oldest kept = %q, want %q", first.Change.Title, want)
	}
	var last forge.Event
	for range buffer - 1 {
		last = recvEvent(t, ch)
	}
	if want := fmt.Sprintf("t%d", total-1); last.Change.Title != want {
		t.Fatalf("newest = %q, want %q", last.Change.Title, want)
	}
}
