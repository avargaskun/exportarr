package fixtures

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// CountingLimiter is an upstream limiter for tests: it counts acquires and
// the slots currently held, and a positive capacity bounds it.
type CountingLimiter struct {
	slots    chan struct{}
	held     ConcurrencyTracker
	acquires atomic.Int64
}

// NewCountingLimiter returns a limiter with capacity slots; 0 means unbounded.
func NewCountingLimiter(capacity int) *CountingLimiter {
	l := &CountingLimiter{}
	if capacity > 0 {
		l.slots = make(chan struct{}, capacity)
	}
	return l
}

// Acquire takes a slot, waiting until one is free or ctx ends. The returned
// release is idempotent.
func (l *CountingLimiter) Acquire(ctx context.Context) (func(), error) {
	if l.slots != nil {
		select {
		case l.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for an upstream request slot: %w", ctx.Err())
		}
	}
	l.acquires.Add(1)
	l.held.enter()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.held.leave()
			if l.slots != nil {
				<-l.slots
			}
		})
	}, nil
}

// Acquires returns the number of successful acquires.
func (l *CountingLimiter) Acquires() int64 { return l.acquires.Load() }

// Outstanding returns the number of slots currently held.
func (l *CountingLimiter) Outstanding() int64 { return l.held.Current() }

// Peak returns the highest number of slots held at once.
func (l *CountingLimiter) Peak() int64 { return l.held.Peak() }
