package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
)

func mustRead(t *testing.T, c *Cache, k forge.Key, o ReadOpts) Result {
	t.Helper()
	res, err := c.Read(context.Background(), k, o)
	if err != nil {
		t.Fatalf("Read(%s): %v", k, err)
	}
	return res
}

func maxAge(d time.Duration) ReadOpts { return ReadOpts{MaxAge: d, MaxAgeSet: true} }

func wantCalls(t *testing.T, f interface{ Calls(string) int }, method string, want int) {
	t.Helper()
	if got := f.Calls(method); got != want {
		t.Fatalf("%s calls = %d, want %d", method, got, want)
	}
}

func TestReadFreshnessWindow(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)

	res := mustRead(t, c, k, ReadOpts{})
	if len(res.Changes) != 1 || res.Changes[0].Number != 1 {
		t.Fatalf("changes = %+v", res.Changes)
	}
	if !res.FetchedAt.Equal(clk.Now()) {
		t.Fatalf("FetchedAt = %v, want %v", res.FetchedAt, clk.Now())
	}
	wantCalls(t, fake, "GetMany", 1)

	mustRead(t, c, k, ReadOpts{})
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(29 * time.Second)
	mustRead(t, c, k, ReadOpts{})
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(time.Second)
	mustRead(t, c, k, ReadOpts{})
	wantCalls(t, fake, "GetMany", 2)
}

func TestReadMaxAgeZeroAlwaysFetches(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)

	for i := 1; i <= 3; i++ {
		mustRead(t, c, k, maxAge(0))
		wantCalls(t, fake, "GetMany", i)
	}
}

func TestReadSingleflight(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	fake.Gate()

	const n = 20
	results := make([]Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			results[i], errs[i] = c.Read(context.Background(), k, ReadOpts{})
		})
	}

	select {
	case <-fake.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream call entered")
	}
	// Let the other readers reach the in-flight fetch before releasing it.
	time.Sleep(50 * time.Millisecond)
	fake.Release()
	wg.Wait()

	wantCalls(t, fake, "GetMany", 1)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("reader %d: %v", i, errs[i])
		}
		if len(results[i].Changes) != 1 || results[i].Changes[0].Number != 1 {
			t.Fatalf("reader %d changes = %+v", i, results[i].Changes)
		}
	}
}

func TestReadNegativeByBranch(t *testing.T) {
	c, clk, fake := newTestCache(t)
	k := forge.ByBranch(repoA, "feature")

	res := mustRead(t, c, k, maxAge(time.Minute))
	if !res.Negative || len(res.Changes) != 0 {
		t.Fatalf("result = %+v, want negative and empty", res)
	}
	wantCalls(t, fake, "ByBranch", 1)

	clk.Advance(10 * time.Second)
	mustRead(t, c, k, maxAge(time.Minute))
	wantCalls(t, fake, "ByBranch", 1)

	clk.Advance(6 * time.Second)
	mustRead(t, c, k, maxAge(time.Minute))
	wantCalls(t, fake, "ByBranch", 2)
}

func TestReadByBranchPositive(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{withBranch(openChange(repoA, 1), "feature")})

	res := mustRead(t, c, forge.ByBranch(repoA, "feature"), ReadOpts{})
	if res.Negative || len(res.Changes) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestReadTerminalChange(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{withState(openChange(repoA, 1), forge.StateMerged)})
	k := forge.ChangeKey(repoA, 1)

	mustRead(t, c, k, maxAge(30*time.Second))
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(45 * time.Minute)
	mustRead(t, c, k, maxAge(30*time.Second))
	wantCalls(t, fake, "GetMany", 1)

	mustRead(t, c, k, maxAge(0))
	wantCalls(t, fake, "GetMany", 2)

	clk.Advance(61 * time.Minute)
	mustRead(t, c, k, maxAge(30*time.Second))
	wantCalls(t, fake, "GetMany", 3)
}

func TestReadUnsupportedProvider(t *testing.T) {
	c, _, fake := newTestCache(t)
	other := forge.RepoRef{Forge: "gitlab", Host: "gitlab.com", Owner: "o", Name: "x"}

	for _, k := range []forge.Key{forge.ChangeKey(other, 1), forge.RepoOpen(other), forge.ByBranch(other, "b")} {
		_, err := c.Read(context.Background(), k, ReadOpts{})
		if !errors.Is(err, forge.ErrUnsupported) {
			t.Fatalf("Read(%s) err = %v, want ErrUnsupported", k, err)
		}
	}
	wantCalls(t, fake, "GetMany", 0)
	wantCalls(t, fake, "ListOpen", 0)
}

func TestReadAbsentChangeIsNegative(t *testing.T) {
	c, clk, fake := newTestCache(t)
	k := forge.ChangeKey(repoA, 99)

	_, err := c.Read(context.Background(), k, maxAge(time.Minute))
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(10 * time.Second)
	_, err = c.Read(context.Background(), k, maxAge(time.Minute))
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	wantCalls(t, fake, "GetMany", 1)

	clk.Advance(6 * time.Second)
	_, _ = c.Read(context.Background(), k, maxAge(time.Minute))
	wantCalls(t, fake, "GetMany", 2)
}

func TestReadPerRefOutcomes(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	fake.SetChanges(repoB, []forge.Change{openChange(repoB, 1)})
	ka, kb := forge.ChangeKey(repoA, 1), forge.ChangeKey(repoB, 1)

	mustRead(t, c, ka, ReadOpts{})

	boom := errors.New("boom")
	fake.FailRepo(repoA, boom)

	var wg sync.WaitGroup
	var resB Result
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = c.Read(context.Background(), ka, maxAge(0))
	}()
	go func() {
		defer wg.Done()
		resB, errB = c.Read(context.Background(), kb, maxAge(0))
	}()
	wg.Wait()

	if !errors.Is(errA, boom) {
		t.Fatalf("repo A err = %v, want boom", errA)
	}
	if errB != nil || len(resB.Changes) != 1 || resB.Changes[0].Repo != repoB {
		t.Fatalf("repo B result = %+v err = %v", resB, errB)
	}

	res := mustRead(t, c, ka, maxAge(time.Hour))
	if len(res.Changes) != 1 || res.Changes[0].Number != 1 {
		t.Fatalf("cached A value lost: %+v", res)
	}
}

func TestReadListFillsChangeEntries(t *testing.T) {
	c, _, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})

	res := mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	if len(res.Changes) != 2 {
		t.Fatalf("list = %+v", res.Changes)
	}
	wantCalls(t, fake, "ListOpen", 1)

	got := mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	if len(got.Changes) != 1 || got.Changes[0].Number != 1 {
		t.Fatalf("member read = %+v", got.Changes)
	}
	wantCalls(t, fake, "GetMany", 0)
	wantCalls(t, fake, "Get", 0)
}

func TestReadListResolvesMembersAtReadTime(t *testing.T) {
	c, clk, fake := newTestCache(t)
	t0 := clk.Now()
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})
	list := forge.RepoOpen(repoA)

	mustRead(t, c, list, ReadOpts{})

	clk.Advance(10 * time.Second)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), withTitle(openChange(repoA, 2), "renamed")})
	mustRead(t, c, forge.ChangeKey(repoA, 2), maxAge(0))
	wantCalls(t, fake, "GetMany", 1)

	res := mustRead(t, c, list, maxAge(time.Hour))
	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", 1)

	titles := map[int]string{}
	for _, ch := range res.Changes {
		titles[ch.Number] = ch.Title
	}
	if titles[2] != "renamed" || titles[1] != "change" {
		t.Fatalf("titles = %v", titles)
	}
	if !res.FetchedAt.Equal(t0) {
		t.Fatalf("FetchedAt = %v, want oldest %v", res.FetchedAt, t0)
	}
}
