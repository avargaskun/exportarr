package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Limiter bounds concurrent upstream HTTP attempts. Acquire blocks until a
// slot is free or ctx ends; the returned release is idempotent.
type Limiter interface {
	Acquire(ctx context.Context) (release func(), err error)
}

// SlotPool is the process-wide cap on upstream requests: one reserved slot
// per target plus a shared pool of capacity-len(targets) slots. It is also a
// prometheus.Collector for its own metrics.
type SlotPool struct {
	shared   chan struct{}
	limiters map[string]*slotLimiter

	max      prometheus.Gauge
	inFlight *prometheus.GaugeVec
	wait     *prometheus.HistogramVec
	timeouts *prometheus.CounterVec
}

// NewSlotPool returns a pool of capacity slots for the named targets. It
// fails when capacity leaves no shared slot or a name repeats.
func NewSlotPool(capacity int, targets []string) (*SlotPool, error) {
	if capacity < len(targets)+1 {
		return nil, fmt.Errorf("upstream request capacity %d is below the number of targets + 1 (%d)", capacity, len(targets)+1)
	}
	p := &SlotPool{
		shared:   make(chan struct{}, capacity-len(targets)),
		limiters: make(map[string]*slotLimiter, len(targets)),
		max: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "exportarr_upstream_requests_max",
			Help: "Maximum number of concurrent upstream HTTP requests across all targets.",
		}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "exportarr_upstream_requests_in_flight",
			Help: "Upstream HTTP requests currently holding a slot.",
		}, []string{"target"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "exportarr_upstream_slot_wait_seconds",
			Help:    "Time upstream HTTP requests waited for a slot.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30},
		}, []string{"target"}),
		timeouts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "exportarr_upstream_slot_wait_timeouts_total",
			Help: "Upstream HTTP requests abandoned while waiting for a slot.",
		}, []string{"target"}),
	}
	p.max.Set(float64(capacity))
	for _, name := range targets {
		if _, ok := p.limiters[name]; ok {
			return nil, fmt.Errorf("duplicate target name %q", name)
		}
		p.limiters[name] = &slotLimiter{
			shared:   p.shared,
			reserved: make(chan struct{}, 1),
			inFlight: p.inFlight.WithLabelValues(name),
			wait:     p.wait.WithLabelValues(name),
			timeouts: p.timeouts.WithLabelValues(name),
		}
	}
	return p, nil
}

// For returns the limiter of the named target. It panics on a name the pool
// was not built with.
func (p *SlotPool) For(target string) Limiter {
	l, ok := p.limiters[target]
	if !ok {
		panic(fmt.Sprintf("client: no upstream slot for target %q", target))
	}
	return l
}

// Describe implements prometheus.Collector.
func (p *SlotPool) Describe(ch chan<- *prometheus.Desc) {
	p.max.Describe(ch)
	p.inFlight.Describe(ch)
	p.wait.Describe(ch)
	p.timeouts.Describe(ch)
}

// Collect implements prometheus.Collector.
func (p *SlotPool) Collect(ch chan<- prometheus.Metric) {
	p.max.Collect(ch)
	p.inFlight.Collect(ch)
	p.wait.Collect(ch)
	p.timeouts.Collect(ch)
}

type slotLimiter struct {
	shared   chan struct{}
	reserved chan struct{}
	inFlight prometheus.Gauge
	wait     prometheus.Observer
	timeouts prometheus.Counter
}

func (l *slotLimiter) Acquire(ctx context.Context) (func(), error) {
	start := time.Now()
	// Taking the reserved slot first leaves shared capacity to other targets.
	select {
	case l.reserved <- struct{}{}:
		return l.hold(l.reserved, start), nil
	default:
	}
	select {
	case l.reserved <- struct{}{}:
		return l.hold(l.reserved, start), nil
	case l.shared <- struct{}{}:
		return l.hold(l.shared, start), nil
	case <-ctx.Done():
		l.timeouts.Inc()
		return nil, fmt.Errorf("waiting for an upstream request slot: %w", ctx.Err())
	}
}

func (l *slotLimiter) hold(slot chan struct{}, start time.Time) func() {
	l.inFlight.Inc()
	l.wait.Observe(time.Since(start).Seconds())
	var once sync.Once
	return func() {
		once.Do(func() {
			l.inFlight.Dec()
			<-slot
		})
	}
}
