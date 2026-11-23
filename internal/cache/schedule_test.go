package cache

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/forgetest"
)

var upstreamMethods = []string{"ListOpen", "Get", "GetMany", "ByBranch", "Search", "Probe"}

func totalCalls(f *forgetest.Fake) int {
	n := 0
	for _, m := range upstreamMethods {
		n += f.Calls(m)
	}
	return n
}

func withLease(d time.Duration) func(*Options) {
	return func(o *Options) { o.LeaseTTL = d }
}

func withEvict(d time.Duration) func(*Options) {
	return func(o *Options) { o.EvictAfter = d }
}

func pendingChange(r forge.RepoRef, n int) forge.Change {
	return withRollup(openChange(r, n), forge.RollupPending)
}

func tick(c *Cache, clk interface{ Advance(time.Duration) }, d time.Duration) {
	clk.Advance(d)
	c.Tick(context.Background())
}

func byNumber(t *testing.T, res Result, n int) forge.Change {
	t.Helper()
	for _, ch := range res.Changes {
		if ch.Number == n {
			return ch
		}
	}
	t.Fatalf("change #%d not in result %+v", n, res.Changes)
	return forge.Change{}
}

func TestSchedLeaseLapseStopsPolling(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	wantCalls(t, fake, "GetMany", 1)

	tick(c, clk, DefaultOptions().LeaseTTL+time.Second)
	if got := totalCalls(fake); got != 1 {
		t.Fatalf("upstream calls = %d after lease lapse, want 1", got)
	}
}

func TestSchedTierStats(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1), openChange(repoA, 2)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	mustRead(t, c, forge.ChangeKey(repoA, 2), ReadOpts{})

	c.Tick(context.Background())
	if s := c.Stats(); s.Hot != 1 || s.Warm != 1 || s.Cold != 0 || s.Keys != 2 {
		t.Fatalf("stats = %+v, want hot 1 warm 1 cold 0 keys 2", s)
	}

	tick(c, clk, DefaultOptions().LeaseTTL+time.Second)
	if s := c.Stats(); s.Hot != 0 || s.Warm != 0 || s.Cold != 2 || s.Keys != 2 {
		t.Fatalf("stats after lapse = %+v, want cold 2 keys 2", s)
	}
}

func TestSchedHotPendingRefetchesAtHotInterval(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})

	tick(c, clk, 24*time.Second)
	wantCalls(t, fake, "GetMany", 1)

	tick(c, clk, time.Second)
	wantCalls(t, fake, "GetMany", 2)
}

func TestSchedHotChangesShareOneGetMany(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1), pendingChange(repoA, 2)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	mustRead(t, c, forge.ChangeKey(repoA, 2), ReadOpts{})
	wantCalls(t, fake, "GetMany", 2)

	tick(c, clk, 25*time.Second)
	wantCalls(t, fake, "GetMany", 3)
	refs := fake.GetManyRefs()
	want := []forge.ChangeRef{{Repo: repoA, Number: 1}, {Repo: repoA, Number: 2}}
	if got := refs[len(refs)-1]; !slices.Equal(got, want) {
		t.Fatalf("last GetMany refs = %v, want %v", got, want)
	}
}

func TestSchedHotMergeableUnknown(t *testing.T) {
	c, clk, fake := newTestCache(t)
	ch := openChange(repoA, 1)
	ch.Mergeable = forge.MergeableUnknown
	fake.SetChanges(repoA, []forge.Change{ch})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})

	tick(c, clk, 25*time.Second)
	wantCalls(t, fake, "GetMany", 2)
}

func TestSchedHotFocusRead(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{Focus: true})
	mustRead(t, c, forge.ChangeKey(repoA, 2), ReadOpts{})
	wantCalls(t, fake, "GetMany", 2)

	tick(c, clk, 25*time.Second)
	wantCalls(t, fake, "GetMany", 3)
	refs := fake.GetManyRefs()
	want := []forge.ChangeRef{{Repo: repoA, Number: 1}}
	if got := refs[len(refs)-1]; !slices.Equal(got, want) {
		t.Fatalf("last GetMany refs = %v, want only the focused change %v", got, want)
	}
}

func TestSchedWarmProbeUnchangedRevalidates(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	wantCalls(t, fake, "ListOpen", 1)
	probes, getMany := fake.Calls("Probe"), fake.Calls("GetMany")

	fake.SetProbe(repoA, false)
	tick(c, clk, 2*time.Minute)
	if got := fake.Calls("Probe") - probes; got != 1 {
		t.Fatalf("probes = %d, want 1", got)
	}
	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", getMany)

	res := mustRead(t, c, forge.RepoOpen(repoA), maxAge(30*time.Second))
	wantCalls(t, fake, "ListOpen", 1)
	wantCalls(t, fake, "GetMany", getMany)
	if !res.FetchedAt.Equal(clk.Now()) {
		t.Fatalf("FetchedAt = %v, want revalidation time %v", res.FetchedAt, clk.Now())
	}
}

func TestSchedWarmProbeChangedRefetchesList(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})

	fake.SetChanges(repoA, []forge.Change{withTitle(openChange(repoA, 1), "new")})
	fake.SetProbe(repoA, true)
	tick(c, clk, 2*time.Minute)
	wantCalls(t, fake, "ListOpen", 2)

	res := mustRead(t, c, forge.RepoOpen(repoA), maxAge(30*time.Second))
	wantCalls(t, fake, "ListOpen", 2)
	if got := byNumber(t, res, 1).Title; got != "new" {
		t.Fatalf("title = %q, want new", got)
	}
}

func TestSchedOneProbePerRepoPerTick(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{withBranch(openChange(repoA, 1), "x")})
	fake.SetChanges(repoB, []forge.Change{openChange(repoB, 1)})
	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	mustRead(t, c, forge.ByBranch(repoA, "x"), ReadOpts{})
	mustRead(t, c, forge.ChangeKey(repoB, 1), ReadOpts{})
	probes := fake.Calls("Probe")

	tick(c, clk, 2*time.Minute)
	if got := fake.Calls("Probe") - probes; got != 2 {
		t.Fatalf("probes = %d, want 2 (one per repo)", got)
	}
}

func TestSchedWarmMaxAgeForcesRefetch(t *testing.T) {
	c, clk, fake := newTestCache(t, withLease(time.Hour))
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	fake.SetProbe(repoA, false)

	for range 4 {
		tick(c, clk, 2*time.Minute)
	}
	wantCalls(t, fake, "ListOpen", 1)

	fake.SetChanges(repoA, []forge.Change{withTitle(openChange(repoA, 1), "hidden")})
	tick(c, clk, 2*time.Minute)
	tick(c, clk, 2*time.Minute)
	wantCalls(t, fake, "ListOpen", 2)
	res := mustRead(t, c, forge.RepoOpen(repoA), maxAge(30*time.Second))
	if got := byNumber(t, res, 1).Title; got != "hidden" {
		t.Fatalf("title = %q, want hidden", got)
	}
}

func TestSchedRepoOpenPendingMemberRefetchedWithoutProbeChange(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1), openChange(repoA, 2)})
	t0 := clk.Now()
	mustRead(t, c, forge.RepoOpen(repoA), ReadOpts{})
	wantCalls(t, fake, "ListOpen", 1)
	getMany := fake.Calls("GetMany")

	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})
	fake.SetProbe(repoA, false)
	tick(c, clk, 25*time.Second)
	wantCalls(t, fake, "GetMany", getMany+1)
	wantCalls(t, fake, "ListOpen", 1)
	refs := fake.GetManyRefs()
	want := []forge.ChangeRef{{Repo: repoA, Number: 1}}
	if got := refs[len(refs)-1]; !slices.Equal(got, want) {
		t.Fatalf("last GetMany refs = %v, want %v", got, want)
	}

	res := mustRead(t, c, forge.RepoOpen(repoA), maxAge(30*time.Second))
	wantCalls(t, fake, "GetMany", getMany+1)
	wantCalls(t, fake, "ListOpen", 1)
	if got := byNumber(t, res, 1).Checks.Rollup; got != forge.RollupPass {
		t.Fatalf("rollup = %q, want pass", got)
	}
	if !res.FetchedAt.Equal(t0) {
		t.Fatalf("FetchedAt = %v, want oldest part %v", res.FetchedAt, t0)
	}
}

func TestSchedBackoffDoublesToCapAndResets(t *testing.T) {
	c, clk, fake := newTestCache(t, withLease(24*time.Hour), withEvict(48*time.Hour))
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	fake.FailAll(errors.New("boom"))

	calls := fake.Calls("GetMany")
	for _, iv := range []time.Duration{
		25 * time.Second, 50 * time.Second, 100 * time.Second, 200 * time.Second,
		400 * time.Second, 10 * time.Minute, 10 * time.Minute,
	} {
		tick(c, clk, iv-time.Second)
		wantCalls(t, fake, "GetMany", calls)
		tick(c, clk, time.Second)
		calls++
		wantCalls(t, fake, "GetMany", calls)
	}

	fake.FailAll(nil)
	tick(c, clk, 10*time.Minute)
	calls++
	wantCalls(t, fake, "GetMany", calls)

	tick(c, clk, 25*time.Second)
	calls++
	wantCalls(t, fake, "GetMany", calls)
}

func TestSchedRateLimitPausesProvider(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1)})
	k := forge.ChangeKey(repoA, 1)
	mustRead(t, c, k, ReadOpts{})

	clk.Advance(25 * time.Second)
	retryAfter := clk.Now().Add(3 * time.Minute)
	fake.SetError("GetMany", &forge.RateLimitError{RetryAfter: retryAfter})
	c.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 2)
	paused := totalCalls(fake)

	tick(c, clk, time.Minute)
	if got := totalCalls(fake); got != paused {
		t.Fatalf("calls while rate limited = %d, want %d", got, paused)
	}
	var rl *forge.RateLimitError
	if _, err := c.Read(context.Background(), k, ReadOpts{}); !errors.As(err, &rl) {
		t.Fatalf("stale Read err = %v, want RateLimitError", err)
	}
	if got := totalCalls(fake); got != paused {
		t.Fatalf("calls from stale Read = %d, want %d", got, paused)
	}

	fake.SetError("GetMany", nil)
	clk.Advance(retryAfter.Sub(clk.Now()) + time.Second)
	mustRead(t, c, k, ReadOpts{})
	wantCalls(t, fake, "GetMany", 3)
	tick(c, clk, 25*time.Second)
	wantCalls(t, fake, "GetMany", 4)
}

func TestSchedPerRefRateLimitPausesProvider(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{pendingChange(repoA, 1)})
	fake.SetChanges(repoB, []forge.Change{pendingChange(repoB, 1)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	kb := forge.ChangeKey(repoB, 1)
	mustRead(t, c, kb, ReadOpts{})
	wantCalls(t, fake, "GetMany", 2)

	clk.Advance(25 * time.Second)
	fake.FailRepo(repoA, &forge.RateLimitError{RetryAfter: clk.Now().Add(3 * time.Minute)})
	c.Tick(context.Background())
	wantCalls(t, fake, "GetMany", 3)
	paused := totalCalls(fake)

	tick(c, clk, time.Minute)
	if got := totalCalls(fake); got != paused {
		t.Fatalf("calls while rate limited = %d, want %d (repo B is paused too)", got, paused)
	}
	var rl *forge.RateLimitError
	if _, err := c.Read(context.Background(), kb, ReadOpts{}); !errors.As(err, &rl) {
		t.Fatalf("stale Read err = %v, want RateLimitError", err)
	}
	if got := totalCalls(fake); got != paused {
		t.Fatalf("calls from stale Read = %d, want %d", got, paused)
	}
}

func TestSchedEvictsEntriesUnreadForEvictAfter(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetChanges(repoA, []forge.Change{openChange(repoA, 1), openChange(repoA, 2)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	clk.Advance(30 * time.Minute)
	mustRead(t, c, forge.ChangeKey(repoA, 2), ReadOpts{})
	c.Tick(context.Background())
	if got := c.Stats().Keys; got != 2 {
		t.Fatalf("keys = %d, want 2", got)
	}

	tick(c, clk, 31*time.Minute)
	if got := c.Stats().Keys; got != 1 {
		t.Fatalf("keys = %d after eviction, want 1", got)
	}
}

func TestSchedQueryRefetchesEveryWarmInterval(t *testing.T) {
	c, clk, fake := newTestCache(t)
	fake.SetSearch("q", []forge.Change{openChange(repoA, 1)})
	mustRead(t, c, forge.Query("github", "q", 10), ReadOpts{})
	wantCalls(t, fake, "Search", 1)

	tick(c, clk, 2*time.Minute-time.Second)
	wantCalls(t, fake, "Search", 1)
	tick(c, clk, time.Second)
	wantCalls(t, fake, "Search", 2)
	tick(c, clk, 2*time.Minute)
	wantCalls(t, fake, "Search", 3)
}

func TestSchedTerminalChangePolledAfterTerminalTTL(t *testing.T) {
	c, clk, fake := newTestCache(t, withLease(3*time.Hour), withEvict(6*time.Hour))
	fake.SetChanges(repoA, []forge.Change{withState(openChange(repoA, 1), forge.StateMerged)})
	mustRead(t, c, forge.ChangeKey(repoA, 1), ReadOpts{})
	wantCalls(t, fake, "GetMany", 1)

	tick(c, clk, 59*time.Minute+59*time.Second)
	wantCalls(t, fake, "GetMany", 1)
	tick(c, clk, time.Second)
	wantCalls(t, fake, "GetMany", 2)
}
