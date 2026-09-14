package fixtures

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/assert"
	"github.com/onedr0p/exportarr/internal/client"
)

var _ client.Limiter = (*CountingLimiter)(nil)

func TestCountingLimiter_Unbounded(t *testing.T) {
	l := NewCountingLimiter(0)
	var releases []func()
	for range 5 {
		release, err := l.Acquire(context.Background())
		assert.NoError(t, err)
		releases = append(releases, release)
	}
	assert.Equal(t, l.Acquires(), int64(5))
	assert.Equal(t, l.Outstanding(), int64(5))
	for _, release := range releases {
		release()
	}
	assert.Equal(t, l.Outstanding(), int64(0))
	assert.Equal(t, l.Peak(), int64(5))
}

func TestCountingLimiter_BoundedWaitsAndCancels(t *testing.T) {
	l := NewCountingLimiter(1)
	release, err := l.Acquire(context.Background())
	assert.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Contains(t, err.Error(), "waiting for an upstream request slot")
	assert.Equal(t, l.Acquires(), int64(1))

	type result struct {
		release func()
		err     error
	}
	got := make(chan result, 1)
	go func() {
		r, err := l.Acquire(context.Background())
		got <- result{r, err}
	}()
	select {
	case <-got:
		t.Fatal("second acquire must wait for the held slot")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case res := <-got:
		assert.NoError(t, res.err)
		res.release()
	case <-time.After(5 * time.Second):
		t.Fatal("second acquire never got the released slot")
	}
	assert.Equal(t, l.Acquires(), int64(2))
	assert.Equal(t, l.Outstanding(), int64(0))
	assert.Equal(t, l.Peak(), int64(1))
}

func TestCountingLimiter_ReleaseIsIdempotent(t *testing.T) {
	l := NewCountingLimiter(2)
	first, err := l.Acquire(context.Background())
	assert.NoError(t, err)
	second, err := l.Acquire(context.Background())
	assert.NoError(t, err)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(first)
	}
	wg.Wait()
	assert.Equal(t, l.Outstanding(), int64(1))

	third, err := l.Acquire(context.Background())
	assert.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = l.Acquire(ctx)
	assert.Error(t, err, "a repeated release must free only one slot")
	second()
	third()
	assert.Equal(t, l.Outstanding(), int64(0))
}
