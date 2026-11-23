package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/clock"
	"github.com/noamsto/forged/internal/forgetest"
)

func seedOf(clk *clock.Fake) uint64 { return uint64(clk.Now().UnixMicro()) }

func withEventBuffer(n int) func(*Options) {
	return func(o *Options) { o.EventBuffer = n }
}

// produce stores n changes of #1, each differing from the last in Mergeable,
// so every store emits exactly one event. The change must already be cached.
func produce(t *testing.T, c *Cache, fake *forgetest.Fake, n int) {
	t.Helper()
	k := forge.ChangeKey(repoA, 1)
	for i := range n {
		m := forge.MergeableConflict
		if i%2 == 1 {
			m = forge.MergeableClean
		}
		fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), m)})
		mustInvalidate(t, c, k)
	}
}

func cachedFirst(t *testing.T, opts ...func(*Options)) (*Cache, *clock.Fake, *forgetest.Fake) {
	t.Helper()
	c, clk, fake := newTestCache(t, opts...)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	return c, clk, fake
}

func recvN(t *testing.T, ch <-chan forge.ChangeEvent, n int) []forge.ChangeEvent {
	t.Helper()
	evs := make([]forge.ChangeEvent, 0, n)
	for range n {
		evs = append(evs, recvEvent(t, ch))
	}
	return evs
}

func wantConsecutive(t *testing.T, evs []forge.ChangeEvent) {
	t.Helper()
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatalf("seq[%d]=%d after seq[%d]=%d, want consecutive", i, evs[i].Seq, i-1, evs[i-1].Seq)
		}
	}
}

func wantStillOpen(t *testing.T, ch <-chan forge.ChangeEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

func TestEventsMappedWithConsecutiveSeqs(t *testing.T) {
	c, clk, fake := cachedFirst(t)
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA})

	cur := withRollup(openChange(repoA, 1), forge.RollupFail)
	cur.Draft = true
	cur.Mergeable = forge.MergeableConflict
	fake.SetChanges(repoA, []forge.Change{cur})
	mustInvalidate(t, c, forge.ChangeKey(repoA, 1))

	evs := recvN(t, ch, 3)
	wantTypes := []forge.EventType{forge.EventChecksCompleted, forge.EventDraftChanged, forge.EventMergeableChanged}
	for i, ev := range evs {
		if ev.Type != wantTypes[i] {
			t.Fatalf("event %d type = %s, want %s", i, ev.Type, wantTypes[i])
		}
		if !ev.At.Equal(clk.Now()) {
			t.Fatalf("event %d At = %v, want %v", i, ev.At, clk.Now())
		}
	}
	if evs[0].Seq <= seedOf(clk) {
		t.Fatalf("first seq = %d, want above seed %d", evs[0].Seq, seedOf(clk))
	}
	wantConsecutive(t, evs)
	wantNoEvent(t, ch)
}

func TestEventsFirstStoreEmitsNothing(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	wantNoEvent(t, ch)
}

func TestEventsSinceReplaysOnlyNewer(t *testing.T) {
	c, clk, fake := cachedFirst(t)
	produce(t, c, fake, 5)

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Since: seedOf(clk) + 2, HasSince: true})
	evs := recvN(t, ch, 3)
	if evs[0].Seq != seedOf(clk)+3 || evs[0].Type == forge.EventResync {
		t.Fatalf("first replayed = %+v, want seq %d", evs[0], seedOf(clk)+3)
	}
	wantConsecutive(t, evs)
	wantNoEvent(t, ch)
}

func TestEventsSinceBelowOldestResyncs(t *testing.T) {
	c, clk, fake := cachedFirst(t, withEventBuffer(4))
	live, _ := mustSubscribe(t, c, Filter{Repo: repoA})
	produce(t, c, fake, 10)
	all := recvN(t, live, 10)
	oldest := all[6].Seq

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Since: seedOf(clk), HasSince: true})
	evs := recvN(t, ch, 5)
	if evs[0].Type != forge.EventResync || evs[0].Seq != oldest-1 {
		t.Fatalf("first = %+v, want resync at seq %d", evs[0], oldest-1)
	}
	for i, ev := range evs[1:] {
		if ev.Seq != all[6+i].Seq || ev.Type == forge.EventResync {
			t.Fatalf("replayed[%d] = %+v, want seq %d", i, ev, all[6+i].Seq)
		}
	}
	wantStillOpen(t, ch)
}

func TestEventsSinceAtBoundaryNoResync(t *testing.T) {
	c, _, fake := cachedFirst(t, withEventBuffer(4))
	live, _ := mustSubscribe(t, c, Filter{Repo: repoA})
	produce(t, c, fake, 10)
	all := recvN(t, live, 10)

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Since: all[6].Seq - 1, HasSince: true})
	evs := recvN(t, ch, 4)
	if evs[0].Type == forge.EventResync || evs[0].Seq != all[6].Seq {
		t.Fatalf("first = %+v, want seq %d without resync", evs[0], all[6].Seq)
	}
	wantStillOpen(t, ch)
}

func TestEventsLargeReplayDoesNotCloseChannel(t *testing.T) {
	c, clk, fake := cachedFirst(t, withEventBuffer(1000))
	produce(t, c, fake, 600)

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Since: seedOf(clk), HasSince: true})
	evs := recvN(t, ch, 600)
	if evs[0].Type == forge.EventResync {
		t.Fatalf("unexpected resync: %+v", evs[0])
	}
	wantConsecutive(t, evs)
	wantStillOpen(t, ch)
}

func TestEventsRestartedCacheResyncsOldCursor(t *testing.T) {
	c1, clk1, fake1 := cachedFirst(t)
	live, _ := mustSubscribe(t, c1, Filter{Repo: repoA})
	produce(t, c1, fake1, 3)
	last := recvN(t, live, 3)[2].Seq

	clk2 := clock.NewFake(clk1.Now().Add(time.Hour))
	fake2 := forgetest.New()
	o := DefaultOptions()
	o.Clock = clk2
	o.BatchWindow = 0
	c2 := New(map[string]forge.Provider{provKey: fake2}, o)

	ch, _ := mustSubscribe(t, c2, Filter{Repo: repoA, Since: last, HasSince: true})
	ev := recvEvent(t, ch)
	if ev.Type != forge.EventResync || ev.Seq <= last {
		t.Fatalf("first = %+v, want resync with seq above %d", ev, last)
	}

	fake2.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c2, forge.ChangeKey(repoA, 1), ReadOpts{})
	fake2.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	mustInvalidate(t, c2, forge.ChangeKey(repoA, 1))
	if next := recvEvent(t, ch); next.Seq <= last {
		t.Fatalf("restarted seq = %d, want above %d", next.Seq, last)
	}
}

func TestEventsRestartedSeqBaseAboveFirstCache(t *testing.T) {
	c1, clk1, fake1 := cachedFirst(t)
	live, _ := mustSubscribe(t, c1, Filter{Repo: repoA})
	produce(t, c1, fake1, 20)
	last := recvN(t, live, 20)[19].Seq

	clk2 := clock.NewFake(clk1.Now().Add(time.Second))
	if seedOf(clk2) <= last {
		t.Fatalf("test setup: second seed %d not above %d", seedOf(clk2), last)
	}
	fake2 := forgetest.New()
	o := DefaultOptions()
	o.Clock = clk2
	o.BatchWindow = 0
	c2 := New(map[string]forge.Provider{provKey: fake2}, o)
	fake2.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c2, forge.ChangeKey(repoA, 1), ReadOpts{})
	ch, _ := mustSubscribe(t, c2, Filter{Repo: repoA})
	fake2.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	mustInvalidate(t, c2, forge.ChangeKey(repoA, 1))
	if ev := recvEvent(t, ch); ev.Seq <= last {
		t.Fatalf("second cache seq = %d, want above first cache's %d", ev.Seq, last)
	}
}

func TestEventsProducedDuringReplayArriveOnce(t *testing.T) {
	c, clk, fake := cachedFirst(t)
	produce(t, c, fake, 5)

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 20 {
			m := forge.MergeableConflict
			if i%2 == 0 {
				m = forge.MergeableClean
			}
			fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), m)})
			if err := c.Invalidate(context.Background(), forge.ChangeKey(repoA, 1)); err != nil {
				t.Errorf("Invalidate: %v", err)
				return
			}
		}
	})
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Since: seedOf(clk), HasSince: true})
	evs := recvN(t, ch, 25)
	wg.Wait()

	if evs[0].Seq != seedOf(clk)+1 {
		t.Fatalf("first seq = %d, want %d", evs[0].Seq, seedOf(clk)+1)
	}
	wantConsecutive(t, evs)
	wantStillOpen(t, ch)
}

func TestEventsSubscribedChangeIsHot(t *testing.T) {
	c, clk, fake := cachedFirst(t)
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	tick(c, clk, DefaultOptions().HotInterval)

	wantCalls(t, fake, "GetMany", 2)
	if ev := recvEvent(t, ch); ev.Type != forge.EventMergeableChanged {
		t.Fatalf("event = %+v", ev)
	}
}

func TestEventsUnfetchedSubscribedChangeIsDueImmediately(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	c.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 1)
}

func TestEventsRepoSubscriptionLeasesOpenList(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustSubscribe(t, c, Filter{Repo: repoA})

	c.Tick(context.Background())
	wantCalls(t, fake, "ListOpen", 1)

	tick(c, clk, DefaultOptions().LeaseTTL+DefaultOptions().EvictAfter)
	wantCalls(t, fake, "ListOpen", 2)
}

func TestEventsSlowSubscriberIsClosedNotBlocking(t *testing.T) {
	c, _, fake := cachedFirst(t, withEventBuffer(8))
	ch, cancel := mustSubscribe(t, c, Filter{Repo: repoA})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(done)
		produce(t, c, fake, 600)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cache blocked on a subscriber that never reads")
	}

	wg.Wait()
	timeout := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-ch:
		case <-timeout:
			t.Fatal("slow subscriber's channel was not closed")
		}
	}
	cancel()
	cancel()
}
