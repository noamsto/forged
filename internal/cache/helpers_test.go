package cache

import (
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/clock"
	"github.com/noamsto/forged/internal/forgetest"
)

const provKey = "github/github.com"

var (
	repoA = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "a"}
	repoB = forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "b"}
)

func newTestCache(t *testing.T, opts ...func(*Options)) (*Cache, *clock.Fake, *forgetest.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	fake := forgetest.New()
	o := DefaultOptions()
	o.Clock = clk
	o.BatchWindow = 0
	for _, f := range opts {
		f(&o)
	}
	return New(map[string]forge.Provider{provKey: fake}, o), clk, fake
}

func openChange(r forge.RepoRef, n int) forge.Change {
	return forge.Change{
		Repo:   r,
		Number: n,
		Title:  "change",
		State:  forge.StateOpen,
		Checks: forge.Checks{Rollup: forge.RollupPass},
	}
}

func withState(c forge.Change, s forge.State) forge.Change {
	c.State = s
	return c
}

func withRollup(c forge.Change, r forge.Rollup) forge.Change {
	c.Checks.Rollup = r
	return c
}

func withTitle(c forge.Change, title string) forge.Change {
	c.Title = title
	return c
}

func withBranch(c forge.Change, ref string) forge.Change {
	c.Head.Ref = ref
	return c
}
