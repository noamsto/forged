package cache

import (
	"context"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/clock"
	"github.com/noamsto/forged/internal/forgetest"
)

func mustInvalidate(t *testing.T, c *Cache, k forge.Key) {
	t.Helper()
	if err := c.Invalidate(context.Background(), k); err != nil {
		t.Fatalf("Invalidate(%s): %v", k, err)
	}
}

func TestInvalidateChangeFetchesWhenFresh(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)

	mustRead(t, c, k, ReadOpts{})
	wantCalls(t, fake, "GetMany", 1)

	mustInvalidate(t, c, k)
	wantCalls(t, fake, "GetMany", 2)
}

func TestInvalidateChangeMarksHot(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	fake.SetProbe(repoA, false)
	k := forge.ChangeKey(repoA, 1)

	mustRead(t, c, k, ReadOpts{})
	mustInvalidate(t, c, k)
	wantCalls(t, fake, "GetMany", 2)

	clk.Advance(25 * time.Second)
	c.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 3)

	clk.Advance(4*time.Minute + 36*time.Second) // 5m1s after the invalidate
	c.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 3)
}

func TestInvalidateRepoOpenRefetchesListAndStalesEntries(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{
		openChange(repoA, 1),
		withState(openChange(repoA, 3), forge.StateMerged),
	})
	list := forge.RepoOpen(repoA)
	nonMember := forge.ChangeKey(repoA, 3)

	mustRead(t, c, list, ReadOpts{})
	mustRead(t, c, nonMember, ReadOpts{})
	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", 1)

	mustRead(t, c, nonMember, maxAge(time.Hour))
	wantCalls(t, fake, "GetMany", 1)

	mustInvalidate(t, c, list)
	wantCalls(t, fake, "ListOpen", 2)

	mustRead(t, c, nonMember, maxAge(time.Hour))
	wantCalls(t, fake, "GetMany", 2)
}

// heldListOpen answers ListOpen from the snapshot taken at call entry, then
// holds the answer until release closes.
type heldListOpen struct {
	*forgetest.Fake
	entered chan struct{}
	release chan struct{}
}

func (h heldListOpen) ListOpen(ctx context.Context, r forge.RepoRef) ([]forge.Change, error) {
	cs, err := h.Fake.ListOpen(ctx, r)
	close(h.entered)
	<-h.release
	return cs, err
}

func TestInvalidateNotOverwrittenByEarlierStartedListFetch(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	fake := forgetest.New()
	held := heldListOpen{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	o := DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	c := New(map[string]forge.Provider{provKey: held}, o)
	open := openChange(repoA, 1)
	fake.SetChanges(repoA, []forge.Change{open})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	listDone := make(chan error, 1)
	go func() {
		_, err := c.Read(context.Background(), forge.RepoOpen(repoA), ReadOpts{})
		listDone <- err
	}()
	<-held.entered // the list holds #1 open

	clk.Advance(time.Second)
	fake.SetChanges(repoA, []forge.Change{withState(open, forge.StateMerged)})
	mustInvalidate(t, c, k)
	close(held.release)
	if err := <-listDone; err != nil {
		t.Fatalf("list Read: %v", err)
	}

	if got := mustRead(t, c, k, ReadOpts{}).Changes[0].State; got != forge.StateMerged {
		t.Fatalf("state after the earlier-started list stored = %s, want merged", got)
	}
	ev := recvEvent(t, ch)
	if ev.Type != forge.EventStateChanged || ev.Old != "open" || ev.New != "merged" {
		t.Fatalf("event = %+v, want state open→merged", ev)
	}
	wantNoEvent(t, ch)
}

// heldGetMany is heldListOpen whose GetMany likewise answers from its entry
// snapshot and holds until gmRelease closes.
type heldGetMany struct {
	heldListOpen
	gmEntered chan struct{}
	gmRelease chan struct{}
}

func (h heldGetMany) GetMany(ctx context.Context, refs []forge.ChangeRef) (map[forge.ChangeRef]forge.Fetched, error) {
	got, err := h.Fake.GetMany(ctx, refs)
	close(h.gmEntered)
	<-h.gmRelease
	return got, err
}

func TestInvalidateNotDroppedByEarlierStartedListFinishingFirst(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	fake := forgetest.New()
	held := heldGetMany{
		heldListOpen: heldListOpen{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})},
		gmEntered:    make(chan struct{}),
		gmRelease:    make(chan struct{}),
	}
	o := DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	c := New(map[string]forge.Provider{provKey: held}, o)
	open := openChange(repoA, 1)
	fake.SetChanges(repoA, []forge.Change{open})
	k := forge.ChangeKey(repoA, 1)
	c.mu.Lock()
	c.store(clk.Now(), clk.Now(), []forge.Change{open})
	c.mu.Unlock()
	ch, _ := mustSubscribe(t, c, Filter{Repo: repoA, Number: 1})

	listDone := make(chan error, 1)
	go func() {
		_, err := c.Read(context.Background(), forge.RepoOpen(repoA), ReadOpts{})
		listDone <- err
	}()
	<-held.entered // t0: the list holds #1 open

	clk.Advance(time.Second)
	fake.SetChanges(repoA, []forge.Change{withState(open, forge.StateMerged)})
	invDone := make(chan error, 1)
	go func() { invDone <- c.Invalidate(context.Background(), k) }()
	<-held.gmEntered // t1: GetMany holds #1 merged

	clk.Advance(time.Second) // t2
	close(held.release)
	if err := <-listDone; err != nil {
		t.Fatalf("list Read: %v", err)
	}
	close(held.gmRelease)
	if err := <-invDone; err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	if got := mustRead(t, c, k, ReadOpts{}).Changes[0].State; got != forge.StateMerged {
		t.Fatalf("state after the later-started invalidate stored = %s, want merged", got)
	}
	ev := recvEvent(t, ch)
	if ev.Type != forge.EventStateChanged || ev.Old != "open" || ev.New != "merged" {
		t.Fatalf("event = %+v, want state open→merged", ev)
	}
	wantNoEvent(t, ch)
}
