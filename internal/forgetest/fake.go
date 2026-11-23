// Package forgetest provides a scripted forge.Provider for tests.
package forgetest

import (
	"context"
	"slices"
	"sync"

	"github.com/noamsto/forged/forge"
)

// Fake is a forge.Provider whose "remote truth" is set by the test. Safe for
// concurrent use.
type Fake struct {
	mu       sync.Mutex
	changes  map[forge.RepoRef][]forge.Change
	search   map[string][]forge.Change
	probe    map[forge.RepoRef]bool
	failRepo map[forge.RepoRef]error
	failAll  error
	failFn   map[string]error
	calls    map[string]int
	refs     [][]forge.ChangeRef
	gate     chan struct{}
	entered  chan string
}

func New() *Fake {
	return &Fake{
		changes:  map[forge.RepoRef][]forge.Change{},
		search:   map[string][]forge.Change{},
		probe:    map[forge.RepoRef]bool{},
		failRepo: map[forge.RepoRef]error{},
		failFn:   map[string]error{},
		calls:    map[string]int{},
		entered:  make(chan string, 1024),
	}
}

func (f *Fake) SetChanges(r forge.RepoRef, cs []forge.Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes[r] = slices.Clone(cs)
}

func (f *Fake) SetSearch(q string, cs []forge.Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.search[q] = slices.Clone(cs)
}

func (f *Fake) SetProbe(r forge.RepoRef, changed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe[r] = changed
}

// FailRepo makes every call touching r fail with err; GetMany reports it per
// ref instead of failing the whole call. A nil err clears it.
func (f *Fake) FailRepo(r forge.RepoRef, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failRepo, r)
		return
	}
	f.failRepo[r] = err
}

// FailAll makes every method fail with err. A nil err clears it.
func (f *Fake) FailAll(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAll = err
}

// SetError makes one method (e.g. "GetMany") fail the whole call with err. A
// nil err clears it.
func (f *Fake) SetError(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failFn, method)
		return
	}
	f.failFn[method] = err
}

// Calls reports how many times method was entered.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// GetManyRefs returns the refs of each GetMany call, each sorted by repo, number.
func (f *Fake) GetManyRefs() [][]forge.ChangeRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refs)
}

// Gate arms a gate: every call entered afterwards blocks until Release.
func (f *Fake) Gate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		f.gate = make(chan struct{})
	}
}

// Release opens the gate, unblocking current and future calls.
func (f *Fake) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
}

// Entered receives the method name at each call entry, before any gate blocks.
// It is buffered; entries beyond the buffer are dropped.
func (f *Fake) Entered() <-chan string { return f.entered }

// enter records the call and blocks on the gate. It returns the error that
// applies to the whole call, if any.
func (f *Fake) enter(ctx context.Context, method string) error {
	f.mu.Lock()
	f.calls[method]++
	gate := f.gate
	f.mu.Unlock()

	select {
	case f.entered <- method:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	return f.failFn[method]
}

func (f *Fake) Capabilities() forge.Capabilities {
	return forge.Capabilities{ConditionalRequests: true, MaxBatch: 50}
}

func (f *Fake) ListOpen(ctx context.Context, r forge.RepoRef) ([]forge.Change, error) {
	if err := f.enter(ctx, "ListOpen"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failRepo[r]; err != nil {
		return nil, err
	}
	var out []forge.Change
	for _, c := range f.changes[r] {
		if c.IsOpen() {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b forge.Change) int { return b.Number - a.Number })
	return out, nil
}

func (f *Fake) Get(ctx context.Context, r forge.RepoRef, number int) (forge.Change, error) {
	if err := f.enter(ctx, "Get"); err != nil {
		return forge.Change{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failRepo[r]; err != nil {
		return forge.Change{}, err
	}
	if c, ok := f.lookup(r, number); ok {
		return c, nil
	}
	return forge.Change{}, forge.ErrNotFound
}

func (f *Fake) GetMany(ctx context.Context, refs []forge.ChangeRef) (map[forge.ChangeRef]forge.Fetched, error) {
	sorted := slices.Clone(refs)
	slices.SortFunc(sorted, func(a, b forge.ChangeRef) int {
		if c := compareRepo(a.Repo, b.Repo); c != 0 {
			return c
		}
		return a.Number - b.Number
	})
	f.mu.Lock()
	f.refs = append(f.refs, sorted)
	f.mu.Unlock()

	if err := f.enter(ctx, "GetMany"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[forge.ChangeRef]forge.Fetched, len(refs))
	for _, ref := range refs {
		if err := f.failRepo[ref.Repo]; err != nil {
			out[ref] = forge.Fetched{Err: err}
			continue
		}
		if c, ok := f.lookup(ref.Repo, ref.Number); ok {
			out[ref] = forge.Fetched{Change: c}
			continue
		}
		out[ref] = forge.Fetched{Err: forge.ErrNotFound}
	}
	return out, nil
}

func (f *Fake) ByBranch(ctx context.Context, r forge.RepoRef, branch string) ([]forge.Change, error) {
	if err := f.enter(ctx, "ByBranch"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failRepo[r]; err != nil {
		return nil, err
	}
	var out []forge.Change
	for _, c := range f.changes[r] {
		if c.Head.Ref == branch {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b forge.Change) int { return b.Number - a.Number })
	return out, nil
}

func (f *Fake) Search(ctx context.Context, query string, _ int) ([]forge.Change, error) {
	if err := f.enter(ctx, "Search"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.search[query]), nil
}

func (f *Fake) Probe(ctx context.Context, r forge.RepoRef) (forge.ProbeResult, error) {
	if err := f.enter(ctx, "Probe"); err != nil {
		return forge.ProbeResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failRepo[r]; err != nil {
		return forge.ProbeResult{}, err
	}
	return forge.ProbeResult{Changed: f.probe[r]}, nil
}

// lookup requires f.mu held.
func (f *Fake) lookup(r forge.RepoRef, number int) (forge.Change, bool) {
	for _, c := range f.changes[r] {
		if c.Number == number {
			return c, true
		}
	}
	return forge.Change{}, false
}

func compareRepo(a, b forge.RepoRef) int {
	switch {
	case a.String() < b.String():
		return -1
	case a.String() > b.String():
		return 1
	}
	return 0
}
