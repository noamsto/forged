package clock

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFakeNowAdvances(t *testing.T) {
	f := NewFake(t0)
	if !f.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", f.Now(), t0)
	}
	f.Advance(90 * time.Second)
	if want := t0.Add(90 * time.Second); !f.Now().Equal(want) {
		t.Fatalf("Now = %v, want %v", f.Now(), want)
	}
}

func TestFakeAfterFuncFiresOnceWhenCrossed(t *testing.T) {
	f := NewFake(t0)
	fired := 0
	f.AfterFunc(10*time.Second, func() { fired++ })

	f.Advance(9 * time.Second)
	if fired != 0 {
		t.Fatalf("fired before deadline: %d", fired)
	}
	f.Advance(1 * time.Second)
	if fired != 1 {
		t.Fatalf("fired = %d at deadline, want 1", fired)
	}
	f.Advance(time.Hour)
	if fired != 1 {
		t.Fatalf("fired = %d after further advance, want exactly once", fired)
	}
}

func TestFakeFiresInDeadlineOrder(t *testing.T) {
	f := NewFake(t0)
	var got []int
	f.AfterFunc(30*time.Second, func() { got = append(got, 3) })
	f.AfterFunc(10*time.Second, func() { got = append(got, 1) })
	f.AfterFunc(20*time.Second, func() { got = append(got, 2) })

	f.Advance(time.Minute)
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestFakeStopBeforeDeadline(t *testing.T) {
	f := NewFake(t0)
	fired := false
	tm := f.AfterFunc(10*time.Second, func() { fired = true })

	if !tm.Stop() {
		t.Fatal("Stop before deadline = false, want true")
	}
	f.Advance(time.Minute)
	if fired {
		t.Fatal("stopped timer fired")
	}
	if tm.Stop() {
		t.Fatal("second Stop = true, want false")
	}
}

func TestFakeStopAfterFire(t *testing.T) {
	f := NewFake(t0)
	tm := f.AfterFunc(time.Second, func() {})
	f.Advance(time.Second)
	if tm.Stop() {
		t.Fatal("Stop after fire = true, want false")
	}
}

func TestFakePending(t *testing.T) {
	f := NewFake(t0)
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0", f.Pending())
	}
	a := f.AfterFunc(10*time.Second, func() {})
	f.AfterFunc(20*time.Second, func() {})
	if f.Pending() != 2 {
		t.Fatalf("Pending = %d, want 2", f.Pending())
	}
	a.Stop()
	if f.Pending() != 1 {
		t.Fatalf("Pending after Stop = %d, want 1", f.Pending())
	}
	f.Advance(time.Minute)
	if f.Pending() != 0 {
		t.Fatalf("Pending after fire = %d, want 0", f.Pending())
	}
}

func TestFakeCallbackSeesDeadlineTimeAndCanRearm(t *testing.T) {
	f := NewFake(t0)
	var seen []time.Time
	var tick func()
	tick = func() {
		seen = append(seen, f.Now())
		if len(seen) < 3 {
			f.AfterFunc(10*time.Second, tick)
		}
	}
	f.AfterFunc(10*time.Second, tick)

	f.Advance(time.Minute)
	if len(seen) != 3 {
		t.Fatalf("chain fired %d times in one Advance, want 3", len(seen))
	}
	for i, got := range seen {
		if want := t0.Add(time.Duration(i+1) * 10 * time.Second); !got.Equal(want) {
			t.Fatalf("callback %d saw Now = %v, want %v", i, got, want)
		}
	}
	if want := t0.Add(time.Minute); !f.Now().Equal(want) {
		t.Fatalf("Now after Advance = %v, want %v", f.Now(), want)
	}
}

func TestFakeCallbackMayCallClockWithoutDeadlock(t *testing.T) {
	f := NewFake(t0)
	done := make(chan struct{})
	f.AfterFunc(time.Second, func() {
		_ = f.Now()
		_ = f.Pending()
		close(done)
	})
	f.Advance(time.Second)
	select {
	case <-done:
	default:
		t.Fatal("callback did not run synchronously in Advance")
	}
}

func TestRealAfterFuncFiresAndStops(t *testing.T) {
	var c Clock = Real{}
	if time.Since(c.Now()) > time.Minute {
		t.Fatalf("Real.Now far from wall clock: %v", c.Now())
	}

	fired := make(chan struct{})
	c.AfterFunc(time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("real timer did not fire")
	}

	tm := c.AfterFunc(time.Hour, func() { t.Error("stopped timer fired") })
	if !tm.Stop() {
		t.Fatal("Stop before deadline = false, want true")
	}
}
