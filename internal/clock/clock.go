// Package clock is the sole source of time for Flowbee (DESIGN §6.3.3: "Flowbee
// is the sole clock"). The deterministic core never calls time.Now() directly;
// the runtime reads the clock and passes the instant into engine.Decide.
package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

// Real is the production clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Fake is a manually-advanced clock for deterministic tests.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{t: t} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
