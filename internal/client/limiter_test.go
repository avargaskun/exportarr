package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/onedr0p/exportarr/internal/assert"
)

func newTestPool(t *testing.T, capacity int, targets ...string) *SlotPool {
	t.Helper()
	p, err := NewSlotPool(capacity, targets)
	assert.NoError(t, err)
	return p
}

func mustAcquire(t *testing.T, l Limiter) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := l.Acquire(ctx)
	assert.NoError(t, err)
	return release
}

func assertBlocked(t *testing.T, l Limiter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release, err := l.Acquire(ctx)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected the acquire to time out, got %v", err)
	assert.True(t, release == nil, "a failed acquire must not return a release func")
}

func reservedLen(p *SlotPool, target string) int {
	return len(p.limiters[target].reserved)
}

func TestNewSlotPool(t *testing.T) {
	for _, tt := range []struct {
		name     string
		capacity int
		targets  []string
		err      string
	}{
		{name: "one shared slot", capacity: 3, targets: []string{"a", "b"}},
		{name: "no targets", capacity: 1},
		{name: "capacity equals targets", capacity: 2, targets: []string{"a", "b"}, err: "upstream request capacity 2 is below the number of targets + 1 (3)"},
		{name: "zero capacity", capacity: 0, err: "upstream request capacity 0 is below the number of targets + 1 (1)"},
		{name: "negative capacity", capacity: -1, targets: []string{"a"}, err: "upstream request capacity -1 is below the number of targets + 1 (2)"},
		{name: "duplicate name", capacity: 10, targets: []string{"a", "b", "a"}, err: `duplicate target name "a"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewSlotPool(tt.capacity, tt.targets)
			if tt.err != "" {
				assert.Error(t, err)
				assert.Equal(t, err.Error(), tt.err)
				assert.True(t, p == nil, "a failed constructor must return a nil pool")
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, cap(p.shared), tt.capacity-len(tt.targets))
			assert.Equal(t, len(p.limiters), len(tt.targets))
			for _, name := range tt.targets {
				assert.Equal(t, cap(p.limiters[name].reserved), 1)
			}
		})
	}
}

func TestSlotPool_ForPanicsOnUnknownTarget(t *testing.T) {
	p := newTestPool(t, 2, "a")
	assert.True(t, p.For("a") == p.For("a"), "For must return the target's own limiter")

	defer func() {
		r := recover()
		assert.NotNil(t, r, "For must panic on an unknown target")
		assert.Equal(t, fmt.Sprint(r), `client: no upstream slot for target "nope"`)
	}()
	p.For("nope")
}

func TestSlotPool_NeverExceedsCapacity(t *testing.T) {
	const (
		capacity   = 5
		goroutines = 12
		iterations = 100
	)
	names := []string{"a", "b", "c"}
	p := newTestPool(t, capacity, names...)

	var current, peak atomic.Int64
	var wg sync.WaitGroup
	for g := range goroutines {
		l := p.For(names[g%len(names)])
		wg.Go(func() {
			for range iterations {
				release, err := l.Acquire(context.Background())
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				n := current.Add(1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(50 * time.Microsecond)
				current.Add(-1)
				release()
			}
		})
	}
	wg.Wait()

	assert.True(t, peak.Load() <= capacity, "peak %d exceeds capacity %d", peak.Load(), capacity)
	assert.True(t, peak.Load() > 1, "the workers never overlapped (peak %d)", peak.Load())
	assert.Equal(t, len(p.shared), 0)
	fams := gatherPool(t, p)
	for _, name := range names {
		assert.Equal(t, reservedLen(p, name), 0)
		assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_requests_in_flight", name).GetGauge().GetValue(), 0.0)
		wantCount := uint64(goroutines / len(names) * iterations)
		assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_slot_wait_seconds", name).GetHistogram().GetSampleCount(), wantCount)
	}
}

func TestSlotPool_ReservedSlotSurvivesExhaustedSharedPool(t *testing.T) {
	p := newTestPool(t, 4, "a", "b")
	a, b := p.For("a"), p.For("b")

	var held []func()
	for range 3 {
		held = append(held, mustAcquire(t, a))
	}
	assert.Equal(t, len(p.shared), 2)
	assertBlocked(t, a)

	start := time.Now()
	held = append(held, mustAcquire(t, b))
	assert.True(t, time.Since(start) < time.Second, "b's reserved slot must be granted immediately")
	assertBlocked(t, b)

	for _, release := range held {
		release()
	}
	assert.Equal(t, len(p.shared), 0)
	assert.Equal(t, reservedLen(p, "a"), 0)
	assert.Equal(t, reservedLen(p, "b"), 0)
}

func TestSlotPool_PrefersReservedSlot(t *testing.T) {
	p := newTestPool(t, 3, "a", "b")
	a := p.For("a")

	first := mustAcquire(t, a)
	assert.Equal(t, reservedLen(p, "a"), 1)
	assert.Equal(t, len(p.shared), 0, "the first acquire must leave the shared pool alone")

	second := mustAcquire(t, a)
	assert.Equal(t, len(p.shared), 1)

	first()
	assert.Equal(t, reservedLen(p, "a"), 0)
	third := mustAcquire(t, a)
	assert.Equal(t, reservedLen(p, "a"), 1)
	assert.Equal(t, len(p.shared), 1, "a freed reserved slot must be preferred over the shared pool")

	second()
	third()
}

func TestSlotPool_AcquireCancelled(t *testing.T) {
	p := newTestPool(t, 2, "a")
	a := p.For("a")
	r1, r2 := mustAcquire(t, a), mustAcquire(t, a)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	release, err := a.Acquire(ctx)
	assert.True(t, release == nil)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Equal(t, err.Error(), "waiting for an upstream request slot: context deadline exceeded")

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err = a.Acquire(ctx)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)

	fams := gatherPool(t, p)
	assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_slot_wait_timeouts_total", "a").GetCounter().GetValue(), 2.0)
	assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_requests_in_flight", "a").GetGauge().GetValue(), 2.0)
	assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_slot_wait_seconds", "a").GetHistogram().GetSampleCount(), uint64(2))

	r1()
	r2()
	assert.Equal(t, reservedLen(p, "a"), 0)
	assert.Equal(t, len(p.shared), 0, "a timed-out acquire must not leak a slot")
	r3, r4 := mustAcquire(t, a), mustAcquire(t, a)
	r3()
	r4()
}

func TestSlotPool_ReleaseIsIdempotent(t *testing.T) {
	p := newTestPool(t, 3, "a")
	a := p.For("a")
	r0 := mustAcquire(t, a)
	r1, r2 := mustAcquire(t, a), mustAcquire(t, a)
	assert.Equal(t, len(p.shared), 2)

	r1()
	r1()
	assert.Equal(t, len(p.shared), 1, "a double release must free only one slot")
	fams := gatherPool(t, p)
	assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_requests_in_flight", "a").GetGauge().GetValue(), 2.0)

	r2()
	r0()
	assert.Equal(t, len(p.shared), 0)
	assert.Equal(t, reservedLen(p, "a"), 0)
}

func TestSlotPool_Metrics(t *testing.T) {
	p := newTestPool(t, 5, "a", "b")
	reg := prometheus.NewRegistry()
	reg.MustRegister(p)

	const header = `
# HELP exportarr_upstream_requests_in_flight Upstream HTTP requests currently holding a slot.
# TYPE exportarr_upstream_requests_in_flight gauge
exportarr_upstream_requests_in_flight{target="a"} %d
exportarr_upstream_requests_in_flight{target="b"} %d
# HELP exportarr_upstream_requests_max Maximum number of concurrent upstream HTTP requests across all targets.
# TYPE exportarr_upstream_requests_max gauge
exportarr_upstream_requests_max 5
# HELP exportarr_upstream_slot_wait_timeouts_total Upstream HTTP requests abandoned while waiting for a slot.
# TYPE exportarr_upstream_slot_wait_timeouts_total counter
exportarr_upstream_slot_wait_timeouts_total{target="a"} %d
exportarr_upstream_slot_wait_timeouts_total{target="b"} %d
`
	names := []string{
		"exportarr_upstream_requests_in_flight",
		"exportarr_upstream_requests_max",
		"exportarr_upstream_slot_wait_timeouts_total",
	}
	compare := func(inA, inB, timeoutsA, timeoutsB int, countA, countB uint64) {
		t.Helper()
		want := fmt.Sprintf(header, inA, inB, timeoutsA, timeoutsB)
		assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want), names...))
		fams := gatherRegistry(t, reg)
		assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_slot_wait_seconds", "a").GetHistogram().GetSampleCount(), countA)
		assert.Equal(t, poolMetric(t, fams, "exportarr_upstream_slot_wait_seconds", "b").GetHistogram().GetSampleCount(), countB)
	}

	compare(0, 0, 0, 0, 0, 0)

	var held []func()
	for range 4 {
		held = append(held, mustAcquire(t, p.For("a")))
	}
	held = append(held, mustAcquire(t, p.For("b")))
	assertBlocked(t, p.For("b"))
	compare(4, 1, 0, 1, 4, 1)

	for _, release := range held {
		release()
	}
	compare(0, 0, 0, 1, 4, 1)

	fams := gatherRegistry(t, reg)
	var bounds []float64
	for _, b := range poolMetric(t, fams, "exportarr_upstream_slot_wait_seconds", "a").GetHistogram().GetBucket() {
		bounds = append(bounds, b.GetUpperBound())
	}
	assert.DeepEqual(t, bounds, []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30})
	assert.Equal(t, fams["exportarr_upstream_slot_wait_seconds"].GetType(), dto.MetricType_HISTOGRAM)

	problems, err := testutil.CollectAndLint(p)
	assert.NoError(t, err)
	assert.Len(t, problems, 0, "lint problems: %v", problems)
}

func gatherPool(t *testing.T, p *SlotPool) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(p)
	return gatherRegistry(t, reg)
}

func gatherRegistry(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	assert.NoError(t, err)
	fams := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		fams[mf.GetName()] = mf
	}
	return fams
}

func poolMetric(t *testing.T, fams map[string]*dto.MetricFamily, name, target string) *dto.Metric {
	t.Helper()
	for _, m := range fams[name].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "target" && l.GetValue() == target {
				return m
			}
		}
	}
	t.Fatalf("no %s sample for target %q", name, target)
	return nil
}
