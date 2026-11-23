package forgetest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/noamsto/forged/forge"
	"github.com/noamsto/forged/internal/forgetest"
)

func change(r forge.RepoRef, n int, s forge.State, branch string) forge.Change {
	c := forge.Change{Repo: r, Number: n, State: s}
	c.Head.Ref = branch
	return c
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	r := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "r"}
	bad := forge.RepoRef{Forge: "github", Host: "github.com", Owner: "o", Name: "bad"}
	f := forgetest.New()
	f.SetChanges(r, []forge.Change{
		change(r, 1, forge.StateOpen, "a"),
		change(r, 3, forge.StateOpen, "b"),
		change(r, 2, forge.StateMerged, "a"),
	})
	f.SetChanges(bad, []forge.Change{change(bad, 1, forge.StateOpen, "a")})

	open, err := f.ListOpen(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].Number != 3 || open[1].Number != 1 {
		t.Fatalf("ListOpen = %+v, want numbers [3 1]", open)
	}

	f.FailRepo(bad, errors.New("boom"))
	refs := []forge.ChangeRef{{Repo: r, Number: 9}, {Repo: r, Number: 2}, {Repo: bad, Number: 1}}
	got, err := f.GetMany(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got[refs[0]].Err, forge.ErrNotFound) {
		t.Fatalf("missing ref err = %v, want ErrNotFound", got[refs[0]].Err)
	}
	if got[refs[1]].Err != nil || got[refs[1]].Change.Number != 2 {
		t.Fatalf("present ref = %+v", got[refs[1]])
	}
	if got[refs[2]].Err == nil {
		t.Fatal("FailRepo ref has no error")
	}
	rec := f.GetManyRefs()
	if len(rec) != 1 || rec[0][0].Number != 1 || rec[0][2].Number != 9 {
		t.Fatalf("recorded refs not sorted: %+v", rec)
	}

	rl := &forge.RateLimitError{RetryAfter: time.Unix(1, 0)}
	f.SetError("GetMany", rl)
	var target *forge.RateLimitError
	if _, err := f.GetMany(ctx, refs); !errors.As(err, &target) {
		t.Fatalf("GetMany err = %v, want RateLimitError", err)
	}

	if f.Calls("ListOpen") != 1 || f.Calls("GetMany") != 2 || f.Calls("Get") != 0 {
		t.Fatalf("calls: ListOpen=%d GetMany=%d Get=%d", f.Calls("ListOpen"), f.Calls("GetMany"), f.Calls("Get"))
	}
}

func TestGate(t *testing.T) {
	r := forge.RepoRef{Host: "h", Owner: "o", Name: "r"}
	f := forgetest.New()
	f.Gate()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.Probe(context.Background(), r)
	}()
	if m := <-f.Entered(); m != "Probe" {
		t.Fatalf("entered %q", m)
	}
	select {
	case <-done:
		t.Fatal("call finished while gated")
	case <-time.After(20 * time.Millisecond):
	}
	f.Release()
	<-done
}
