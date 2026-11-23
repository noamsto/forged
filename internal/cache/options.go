package cache

import (
	"time"

	"github.com/noamsto/forge-cache/internal/clock"
)

type Options struct {
	DefaultMaxAge time.Duration
	NegativeTTL   time.Duration
	TerminalTTL   time.Duration
	LeaseTTL      time.Duration
	HotInterval   time.Duration
	HotHold       time.Duration
	WarmInterval  time.Duration
	WarmMaxAge    time.Duration
	EvictAfter    time.Duration
	BatchWindow   time.Duration
	Tick          time.Duration
	MaxBackoff    time.Duration
	EventBuffer   int
	Clock         clock.Clock
}

func DefaultOptions() Options {
	return Options{
		DefaultMaxAge: 30 * time.Second,
		NegativeTTL:   15 * time.Second,
		TerminalTTL:   time.Hour,
		LeaseTTL:      10 * time.Minute,
		HotInterval:   25 * time.Second,
		HotHold:       5 * time.Minute,
		WarmInterval:  2 * time.Minute,
		WarmMaxAge:    10 * time.Minute,
		EvictAfter:    time.Hour,
		BatchWindow:   10 * time.Millisecond,
		Tick:          time.Second,
		MaxBackoff:    10 * time.Minute,
		EventBuffer:   4096,
		Clock:         clock.Real{},
	}
}
