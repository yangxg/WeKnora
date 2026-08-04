package academic_search

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultAttempts bounds retries of transient failures. It is small on
	// purpose: every retry spends the registry's rate budget, and on a paced
	// client (arXiv, PubMed) it also delays every later request in the same run.
	DefaultAttempts = 3

	// BaseRetryDelay and MaxRetryDelay bound the backoff when a registry does
	// not say how long to wait.
	BaseRetryDelay = 500 * time.Millisecond
	MaxRetryDelay  = 30 * time.Second
)

// RetryDelay honours the registry's Retry-After and otherwise backs off
// exponentially, capped either way.
//
// The registry's own number wins whenever it gives one: Crossref and NCBI both
// answer an over-rate caller with the delay that would have kept it under, and
// guessing shorter than that is how a rate warning becomes a block.
func RetryDelay(attempt int, retryAfter time.Duration) time.Duration {
	delay := retryAfter
	if delay <= 0 {
		delay = BaseRetryDelay << (attempt - 1)
	}
	if delay > MaxRetryDelay {
		delay = MaxRetryDelay
	}
	return delay
}

// ParseRetryAfter reads the header in either RFC 7231 form: delay-seconds or an
// HTTP date. An unreadable value yields 0, which leaves the caller on its own
// backoff rather than on no wait at all.
func ParseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if delay := time.Until(deadline); delay > 0 {
			return delay
		}
	}
	return 0
}

// HTTPStatusRetryable reports whether a transport status is worth another try.
// See httpStatusRetryable for why 403 is excluded.
func HTTPStatusRetryable(status int) bool { return httpStatusRetryable(status) }

// SleepContext waits for d, or returns early when ctx is done.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
