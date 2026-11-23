package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
)

func recvEvent(t *testing.T, ch <-chan forge.ChangeEvent) forge.ChangeEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return forge.ChangeEvent{}
	}
}

// wantNoEvent is only called after the synchronous call that would publish
// has returned, so an immediate poll is enough.
func wantNoEvent(t *testing.T, ch <-chan forge.ChangeEvent) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	default:
	}
}

func mustSubscribe(t *testing.T, c *Cache, f Filter) (<-chan forge.ChangeEvent, func()) {
	t.Helper()
	ch, cancel, err := c.Subscribe(f)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(cancel)
	return ch, cancel
}

func withMergeable(c forge.Change, m forge.Mergeable) forge.Change {
	c.Mergeable = m
	return c
}

func TestWatchReceivesTypedEvent(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA})

	fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	mustInvalidate(t, c, k)

	ev := recvEvent(t, ch)
	if ev.Type != forge.EventMergeableChanged || ev.Repo != repoA || ev.Number != 1 ||
		ev.New != string(forge.MergeableConflict) || !ev.At.Equal(clk.Now()) {
		t.Fatalf("event = %+v", ev)
	}
	wantNoEvent(t, ch)
}

func TestWatchIdenticalRefetchEmitsNothing(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA})

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

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 2})

	fake.SetChanges(repoA, []forge.Change{
		withMergeable(openChange(repoA, 1), forge.MergeableConflict),
		withMergeable(openChange(repoA, 2), forge.MergeableConflict),
	})
	mustInvalidate(t, c, k1)
	wantNoEvent(t, ch)

	mustInvalidate(t, c, k2)
	if ev := recvEvent(t, ch); ev.Number != 2 || ev.Type != forge.EventMergeableChanged {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchRepoFilterExcludesOtherRepos(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoB, []forge.Change{openChange(repoB, 1)})
	k := forge.ChangeKey(repoB, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA})

	fake.SetChanges(repoB, []forge.Change{withMergeable(openChange(repoB, 1), forge.MergeableConflict)})
	mustInvalidate(t, c, k)
	wantNoEvent(t, ch)
}

func TestWatchTypesFilter(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Types: []forge.EventType{forge.EventDraftChanged}})

	fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	mustInvalidate(t, c, k)
	wantNoEvent(t, ch)

	drafted := withMergeable(openChange(repoA, 1), forge.MergeableConflict)
	drafted.Draft = true
	fake.SetChanges(repoA, []forge.Change{drafted})
	mustInvalidate(t, c, k)
	if ev := recvEvent(t, ch); ev.Type != forge.EventDraftChanged || ev.Old != "ready" || ev.New != "draft" {
		t.Fatalf("event = %+v", ev)
	}
	wantNoEvent(t, ch)
}

func TestWatchKeepsKeyLeasedPastLeaseTTL(t *testing.T) {
	c, clk, fake := newTestCache(t)
	pending := withRollup(openChange(repoA, 1), forge.RollupPending)
	fake.SetChanges(repoA, []forge.Change{pending})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})

	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	fake.SetChanges(repoA, []forge.Change{withRollup(pending, forge.RollupPass)})
	clk.Advance(DefaultOptions().LeaseTTL + time.Second)
	c.Tick(context.Background())

	wantCalls(t, fake, "GetMany", 2)
	if ev := recvEvent(t, ch); ev.Type != forge.EventChecksCompleted || ev.New != string(forge.RollupPass) {
		t.Fatalf("event = %+v", ev)
	}
}

func TestWatchCancelStopsDelivery(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	ch, cancel := mustSubscribe(t, c, Filter{Repo: repoA})
	cancel()

	if _, ok := <-ch; ok {
		t.Fatal("channel delivered after cancel, want closed")
	}
	fake.SetChanges(repoA, []forge.Change{withMergeable(openChange(repoA, 1), forge.MergeableConflict)})
	mustInvalidate(t, c, k)
	wantNoEvent(t, ch)
	cancel()
}

func TestWatchSubscribeUnsupportedHost(t *testing.T) {
	c, _, fake := newTestCache(t)
	gitlab := forge.RepoRef{Forge: "gitlab", Host: "gitlab.com", Owner: "o", Name: "r"}

	ch, cancel, err := c.Subscribe(Filter{Repo: gitlab})
	if !errors.Is(err, forge.ErrUnsupported) {
		t.Fatalf("Subscribe err = %v, want ErrUnsupported", err)
	}
	if ch != nil || cancel != nil {
		t.Fatalf("Subscribe returned ch=%v cancel=%v with an error", ch, cancel != nil)
	}

	c.Tick(context.Background())
	if got := totalCalls(fake); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}
