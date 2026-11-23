package cache

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forge-cache/forge"
	"github.com/noamsto/forge-cache/internal/clock"
	"github.com/noamsto/forge-cache/internal/forgetest"
)

const batchWindow = 10 * time.Millisecond

func newBatchCache(t *testing.T) (*Cache, *clock.Fake, *forgetest.Fake) {
	t.Helper()
	return newTestCache(t, func(o *Options) { o.BatchWindow = batchWindow })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitEntered(t *testing.T, f *forgetest.Fake, method string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-f.Entered():
			if m == method {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s call", method)
		}
	}
}

func openChanges(r forge.RepoRef, numbers ...int) []forge.Change {
	var out []forge.Change
	for _, n := range numbers {
		out = append(out, openChange(r, n))
	}
	return out
}

func seq(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

type readOutcome struct {
	res Result
	err error
}

// readAsync starts a Read in its own goroutine; the outcome arrives on the
// returned channel.
func readAsync(c *Cache, k forge.Key, o ReadOpts) <-chan readOutcome {
	ch := make(chan readOutcome, 1)
	go func() {
		res, err := c.Read(context.Background(), k, o)
		ch <- readOutcome{res, err}
	}()
	return ch
}

// await waits for ch, advancing the fake clock past the batch window whenever
// requests are queued.
func await(t *testing.T, c *Cache, clk *clock.Fake, ch <-chan readOutcome) readOutcome {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case out := <-ch:
			return out
		case <-timeout:
			t.Fatal("timed out waiting for read")
		case <-time.After(time.Millisecond):
			if c.TestBatchPending() > 0 {
				clk.Advance(batchWindow)
			}
		}
	}
}

func readNow(t *testing.T, c *Cache, clk *clock.Fake, k forge.Key, o ReadOpts) readOutcome {
	t.Helper()
	return await(t, c, clk, readAsync(c, k, o))
}

func TestBatchConcurrentReadsOneGetMany(t *testing.T) {
	c, clk, fake := newBatchCache(t)
	fake.SetChanges(repoA, openChanges(repoA, seq(1, 10)...))
	fake.Gate()

	var wg sync.WaitGroup
	got := make([]forge.Change, 10)
	errs := make([]error, 10)
	for n := 1; n <= 10; n++ {
		wg.Go(func() {
			res, err := c.Read(context.Background(), forge.ChangeKey(repoA, n), ReadOpts{})
			errs[n-1] = err
			if err == nil {
				got[n-1] = res.Changes[0]
			}
		})
	}
	eventually(t, "10 pending refs", func() bool { return c.TestBatchPending() == 10 })
	wantCalls(t, fake, "GetMany", 0)

	clk.Advance(batchWindow)
	waitEntered(t, fake, "GetMany")
	fake.Release()
	wg.Wait()

	wantCalls(t, fake, "GetMany", 1)
	refs := fake.GetManyRefs()
	if len(refs) != 1 || len(refs[0]) != 10 {
		t.Fatalf("GetMany refs = %v, want one call with 10 refs", refs)
	}
	for i, ref := range refs[0] {
		if ref.Repo != repoA || ref.Number != i+1 {
			t.Fatalf("ref %d = %v", i, ref)
		}
	}
	for i := range got {
		if errs[i] != nil || got[i].Number != i+1 {
			t.Fatalf("reader %d: change %+v err %v", i+1, got[i], errs[i])
		}
	}
	if p := c.TestBatchPending(); p != 0 {
		t.Fatalf("BatchPending = %d after flush, want 0", p)
	}
}

func TestBatchFlushesAtMaxBatch(t *testing.T) {
	c, clk, fake := newBatchCache(t)
	fake.SetChanges(repoA, openChanges(repoA, seq(1, 60)...))
	fake.Gate()

	var wg sync.WaitGroup
	for n := 1; n <= 60; n++ {
		wg.Go(func() {
			if _, err := c.Read(context.Background(), forge.ChangeKey(repoA, n), ReadOpts{}); err != nil {
				t.Errorf("read %d: %v", n, err)
			}
		})
	}
	waitEntered(t, fake, "GetMany")
	wantCalls(t, fake, "GetMany", 1)
	if refs := fake.GetManyRefs(); len(refs) != 1 || len(refs[0]) != 50 {
		t.Fatalf("first batch = %v, want 50 refs", refs)
	}

	eventually(t, "10 refs left pending", func() bool { return c.TestBatchPending() == 10 })
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(batchWindow)
	waitEntered(t, fake, "GetMany")
	fake.Release()
	wg.Wait()

	wantCalls(t, fake, "GetMany", 2)
	if refs := fake.GetManyRefs(); len(refs[1]) != 10 {
		t.Fatalf("second batch has %d refs, want 10", len(refs[1]))
	}
}

func TestBatchChangeReadRidesInflightRepoOpen(t *testing.T) {
	c, clk, fake := newBatchCache(t)
	fake.SetChanges(repoA, openChanges(repoA, 1, 2, 3))
	fake.Gate()

	list := readAsync(c, forge.RepoOpen(repoA), ReadOpts{})
	waitEntered(t, fake, "ListOpen")
	open := readAsync(c, forge.ChangeKey(repoA, 3), ReadOpts{})
	absent := readAsync(c, forge.ChangeKey(repoA, 99), ReadOpts{})
	wantCalls(t, fake, "GetMany", 0)
	fake.Release()

	if out := await(t, c, clk, list); out.err != nil || len(out.res.Changes) != 3 {
		t.Fatalf("list read: %+v", out)
	}
	if out := await(t, c, clk, open); out.err != nil || out.res.Changes[0].Number != 3 {
		t.Fatalf("change 3 read: %+v", out)
	}
	if out := await(t, c, clk, absent); !errors.Is(out.err, forge.ErrNotFound) {
		t.Fatalf("change 99 err = %v, want ErrNotFound", out.err)
	}

	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", 1)
	want := []forge.ChangeRef{{Repo: repoA, Number: 99}}
	if refs := fake.GetManyRefs(); len(refs) != 1 || !slices.Equal(refs[0], want) {
		t.Fatalf("GetMany refs = %v, want [%v]", refs, want)
	}
}

func TestBatchMemberOnlyRefresh(t *testing.T) {
	c, clk, fake := newBatchCache(t)
	pending := withRollup(openChange(repoA, 1), forge.RollupPending)
	fake.SetChanges(repoA, []forge.Change{pending, openChange(repoA, 2)})
	list := forge.RepoOpen(repoA)

	readNow(t, c, clk, list, ReadOpts{})
	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", 0)

	clk.Advance(20 * time.Second)
	readNow(t, c, clk, forge.ChangeKey(repoA, 2), maxAge(0))
	wantCalls(t, fake, "GetMany", 1)
	if refs := fake.GetManyRefs(); len(refs[0]) != 1 || refs[0][0].Number != 2 {
		t.Fatalf("GetMany refs = %v, want only #2", refs)
	}
	wantCalls(t, fake, "ListOpen", 1)

	clk.Advance(15 * time.Second)
	out := readNow(t, c, clk, list, maxAge(30*time.Second))
	if out.err != nil {
		t.Fatal(out.err)
	}
	wantCalls(t, fake, "ListOpen", 2)
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(5 * time.Second)
	readNow(t, c, clk, list, maxAge(time.Hour))
	wantCalls(t, fake, "ListOpen", 2)
	wantCalls(t, fake, "GetMany", 1)
}

func TestBatchStaleMembersOnlyGoThroughGetMany(t *testing.T) {
	c, clk, fake := newBatchCache(t)
	fake.SetChanges(repoA, openChanges(repoA, 1, 2))
	list := forge.RepoOpen(repoA)

	readNow(t, c, clk, list, ReadOpts{})
	wantCalls(t, fake, "ListOpen", 1)

	c.TestSetFetchedAt(forge.ChangeKey(repoA, 1), clk.Now().Add(-time.Hour))
	out := readNow(t, c, clk, list, maxAge(30*time.Second))
	if out.err != nil || len(out.res.Changes) != 2 {
		t.Fatalf("read: %+v", out)
	}

	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", 1)
	if refs := fake.GetManyRefs(); len(refs) != 1 || len(refs[0]) != 1 || refs[0][0].Number != 1 {
		t.Fatalf("GetMany refs = %v, want only #1", refs)
	}
}
