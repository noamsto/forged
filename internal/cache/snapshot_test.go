package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/clock"
	"github.com/noamsto/forged/internal/forgetest"
)

func newPeerCache(clk *clock.Fake, fake *forgetest.Fake) *Cache {
	o := DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	return New(map[string]forge.Provider{provKey: fake}, o)
}

func saveSnapshot(t *testing.T, c *Cache) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := c.SaveSnapshot(path); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	return path
}

func TestSnapshotRoundTripHonoursOriginalFetchTime(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	t0 := clk.Now()
	mustRead(t, c, k, ReadOpts{})
	path := saveSnapshot(t, c)

	clk.Advance(20 * time.Second)
	c2 := newPeerCache(clk, fake)
	if err := c2.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	res := mustRead(t, c2, k, maxAge(time.Minute))
	wantCalls(t, fake, "GetMany", 1)
	if len(res.Changes) != 1 || res.Changes[0].Number != 1 || !res.FetchedAt.Equal(t0) {
		t.Fatalf("result = %+v, want change 1 fetched at %v", res, t0)
	}

	mustRead(t, c2, k, maxAge(10*time.Second))
	wantCalls(t, fake, "GetMany", 2)
}

func TestSnapshotLoadedAfterClockStepBackStillStores(t *testing.T) {
	c, clk, fake := newTestCache(t)
	open := openChange(repoA, 1)
	fake.SetChanges(repoA, []forge.Change{open})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})
	path := saveSnapshot(t, c)

	c2 := newPeerCache(clock.NewFake(clk.Now().Add(-time.Hour)), fake)
	if err := c2.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	fake.SetChanges(repoA, []forge.Change{withState(open, forge.StateMerged)})
	mustInvalidate(t, c2, k)
	if got := mustRead(t, c2, k, ReadOpts{}).Changes[0].State; got != forge.StateMerged {
		t.Fatalf("state after a fetch older than the snapshot time = %s, want merged", got)
	}
}

func TestSnapshotKeepsNegativeEntries(t *testing.T) {
	c, clk, fake := newTestCache(t)
	k := forge.ChangeKey(repoA, 9)
	if _, err := c.Read(context.Background(), k, ReadOpts{}); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("Read err = %v, want ErrNotFound", err)
	}
	path := saveSnapshot(t, c)

	clk.Advance(5 * time.Second)
	c2 := newPeerCache(clk, fake)
	if err := c2.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, err := c2.Read(context.Background(), k, maxAge(time.Minute)); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("Read err = %v, want ErrNotFound", err)
	}
	wantCalls(t, fake, "GetMany", 1)
}

func TestSnapshotLoadedEntriesAreColdUntilRead(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{withRollup(openChange(repoA, 1), forge.RollupPending)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})
	path := saveSnapshot(t, c)

	c2 := newPeerCache(clk, fake)
	if err := c2.LoadSnapshot(path); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	clk.Advance(25 * time.Second)
	c2.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 1)

	mustRead(t, c2, k, maxAge(10*time.Second))
	wantCalls(t, fake, "GetMany", 2)

	clk.Advance(25 * time.Second)
	c2.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 3)
}

func TestSnapshotWrittenAtomically(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})

	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	for range 2 {
		if err := c.SaveSnapshot(path); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		t.Fatalf("dir holds %v, want only snapshot.json", entries)
	}
}

func TestStatsCountsTiersKeysAndCalls(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{
		withRollup(openChange(repoA, 1), forge.RollupPending),
		openChange(repoA, 2),
	})
	fake.SetChanges(repoB, []forge.Change{openChange(repoB, 1)})

	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	mustRead(t, c, forge.ChangeKey(repoB, 1), ReadOpts{})

	s := c.Stats()
	// keys: repo-open(A), change A#1, A#2, B#1
	if s.Keys != 4 || s.Hot != 1 || s.Warm != 3 || s.Cold != 0 {
		t.Fatalf("stats = %+v, want 4 keys, 1 hot, 3 warm, 0 cold", s)
	}
	if got := s.Calls[provKey+".ListOpen"]; got != 1 {
		t.Fatalf("ListOpen calls = %d, want 1", got)
	}
	if got := s.Calls[provKey+".GetMany"]; got != 1 {
		t.Fatalf("GetMany calls = %d, want 1", got)
	}

	clk.Advance(DefaultOptions().LeaseTTL + time.Second)
	s = c.Stats()
	if s.Keys != 4 || s.Hot != 0 || s.Warm != 0 || s.Cold != 4 {
		t.Fatalf("stats after lease lapse = %+v, want 4 keys, all cold", s)
	}
}

func TestStatsInflight(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	fake.Gate()

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := c.Read(context.Background(), forge.ChangeKey(repoA, 1), ReadOpts{}); err != nil {
			t.Errorf("Read: %v", err)
		}
	})
	select {
	case <-fake.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream call entered")
	}
	if got := c.Stats().Inflight; got != 1 {
		t.Errorf("Inflight = %d, want 1", got)
	}
	fake.Release()
	wg.Wait()
	if got := c.Stats().Inflight; got != 0 {
		t.Errorf("Inflight after read = %d, want 0", got)
	}
}

func TestStatsPausedUntil(t *testing.T) {
	c, clk, fake := newTestCache(t)
	until := clk.Now().Add(time.Minute)
	fake.SetError("GetMany", &forge.RateLimitError{RetryAfter: until})

	_, err := c.Read(context.Background(), forge.ChangeKey(repoA, 1), ReadOpts{})
	var rl *forge.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("Read err = %v, want RateLimitError", err)
	}
	if got := c.Stats().PausedUntil[provKey]; !got.Equal(until) {
		t.Fatalf("PausedUntil = %v, want %v", got, until)
	}
}
