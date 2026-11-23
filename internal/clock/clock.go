// Package clock abstracts time so schedulers can be driven by a fake in tests.
package clock

import "time"

type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type Timer interface {
	// Stop reports whether it prevented the callback from running.
	Stop() bool
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
