package cache

import (
	"context"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
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
