package academic_search

import (
	"context"
	"sync"
	"time"
)

// Pacer enforces a minimum interval between requests.
//
// The intervals it is constructed with are constants inside each client, never
// manifest settings, and that placement is a decision rather than an omission
// (ADR-0012 §5). arXiv's limit is one request every three seconds across *all
// machines under our control*, and PubMed's is a rate tied to a registration
// made offline with NCBI. Those are promises the deployment makes to a registry;
// a rate ceiling a project could edit would be a project-editable way to break
// one, and the penalty is a blocked IP for everyone sharing it.
//
// A process-local pacer cannot make the fleet-wide promise on its own. What it
// can do is make a single connector honour its share, which is why the academic
// connector fans out serially per registry rather than concurrently.
type Pacer struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	started  bool

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewPacer returns a pacer that admits one request every interval. A
// non-positive interval means no pacing.
func NewPacer(interval time.Duration) *Pacer {
	return newPacerWithClock(interval, time.Now, SleepContext)
}

func newPacerWithClock(
	interval time.Duration,
	now func() time.Time,
	sleep func(context.Context, time.Duration) error,
) *Pacer {
	return &Pacer{interval: interval, now: now, sleep: sleep}
}

// Interval reports the minimum spacing this pacer enforces. A nil pacer paces
// nothing and reports zero.
func (p *Pacer) Interval() time.Duration {
	if p == nil {
		return 0
	}
	return p.interval
}

// Wait blocks until the next request may be sent, or until ctx is done.
//
// A nil Pacer is a no-op, so a client that has no documented rate limit can hold
// a nil field rather than a special case.
func (p *Pacer) Wait(ctx context.Context) error {
	if p == nil || p.interval <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	// started rather than last.IsZero(): a zero clock reading is a legitimate
	// time, and treating it as "never called" would let the first two requests
	// through back to back under a test clock.
	if p.started {
		if elapsed := now.Sub(p.last); elapsed < p.interval {
			if err := p.sleep(ctx, p.interval-elapsed); err != nil {
				// The request is not sent, so the clock is not advanced: the
				// next caller still owes the full interval.
				return err
			}
			now = p.now()
		}
	}
	p.last = now
	p.started = true
	return nil
}
