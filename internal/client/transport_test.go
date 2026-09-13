package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/assert"
)

// scriptedTransport returns canned status codes in order, recording the time
// of every attempt; the last status repeats once the script is exhausted.
type scriptedTransport struct {
	mu       sync.Mutex
	statuses []int
	attempts []time.Time
}

func (s *scriptedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statuses[min(len(s.attempts), len(s.statuses)-1)]
	s.attempts = append(s.attempts, time.Now())
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

func TestRoundTrip_BacksOffBetweenRetries(t *testing.T) {
	inner := &scriptedTransport{statuses: []int{http.StatusInternalServerError, http.StatusOK}}
	transport := NewExportarrTransport(inner, nil)
	const wait = 25 * time.Millisecond
	transport.Backoff = func(int) time.Duration { return wait }

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	assert.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	_ = resp.Body.Close()

	assert.Len(t, inner.attempts, 2)
	assert.GreaterOrEqual(t, inner.attempts[1].Sub(inner.attempts[0]), wait)
}

func TestRoundTrip_BackoffRespectsContextCancel(t *testing.T) {
	inner := &scriptedTransport{statuses: []int{http.StatusInternalServerError}}
	transport := NewExportarrTransport(inner, nil)
	transport.Backoff = func(int) time.Duration { return time.Hour }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	assert.NoError(t, err)

	start := time.Now()
	_, err = transport.RoundTrip(req)
	assert.Error(t, err)
	assert.Len(t, inner.attempts, 1, "no retry should happen after cancellation")
	assert.True(t, time.Since(start) < time.Minute, "canceled backoff should return promptly")
}

func TestDefaultBackoff_GrowsWithAttempts(t *testing.T) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		d := defaultBackoff(attempt)
		assert.GreaterOrEqual(t, d, time.Duration(attempt)*retryBaseBackoff)
		assert.True(t, d < time.Duration(attempt)*retryBaseBackoff+retryJitter, "jitter exceeds bound")
	}
}

// queryKeyAuth authenticates like SABnzbd: the key rides in the query string.
type queryKeyAuth struct{}

func (queryKeyAuth) Auth(req *http.Request) error {
	q := req.URL.Query()
	q.Set("apikey", "hunter2")
	req.URL.RawQuery = q.Encode()
	return nil
}

func TestRoundTrip_RedirectErrorRedactsLocation(t *testing.T) {
	for _, location := range []string{
		"#top",
		"https://sab.example.com/api?apikey=hunter2&mode=queue",
		"//user:hunter2@sab.example.com/api",
	} {
		t.Run(location, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
			}))
			defer ts.Close()

			c, err := NewClient(ts.URL, TransportOptions{}, 0, queryKeyAuth{})
			assert.NoError(t, err)
			err = c.DoRequest("api", nil)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "Redirect Status Code: 302")
			assert.NotContains(t, err.Error(), "hunter2")
		})
	}
}

// countingLimiter is a Limiter that counts acquires and releases. A positive
// capacity bounds it; its release is deliberately not idempotent.
type countingLimiter struct {
	slots       chan struct{}
	outstanding atomic.Int64
	acquires    atomic.Int64
	releases    atomic.Int64
}

func newCountingLimiter(capacity int) *countingLimiter {
	l := &countingLimiter{}
	if capacity > 0 {
		l.slots = make(chan struct{}, capacity)
	}
	return l
}

func (l *countingLimiter) Acquire(ctx context.Context) (func(), error) {
	if l.slots != nil {
		select {
		case l.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for an upstream request slot: %w", ctx.Err())
		}
	}
	l.acquires.Add(1)
	l.outstanding.Add(1)
	return func() {
		l.releases.Add(1)
		l.outstanding.Add(-1)
		if l.slots != nil {
			<-l.slots
		}
	}, nil
}

func TestBaseTransport_Limiter(t *testing.T) {
	_, ok := BaseTransport(TransportOptions{}).(*http.Transport)
	assert.True(t, ok, "without a limiter BaseTransport must return the *http.Transport itself")

	lim := newCountingLimiter(0)
	rt := BaseTransport(TransportOptions{Limiter: lim, InsecureSkipVerify: true, ProxyFromEnvironment: true})
	limited, ok := rt.(*limitedTransport)
	assert.True(t, ok, "with a limiter BaseTransport must return a *limitedTransport, got %T", rt)
	assert.True(t, limited.limiter == Limiter(lim))
	inner, ok := limited.inner.(*http.Transport)
	assert.True(t, ok, "the limited transport must wrap the *http.Transport, got %T", limited.inner)
	assert.True(t, inner.TLSClientConfig.InsecureSkipVerify)
	assert.True(t, inner.Proxy != nil)
	assert.Equal(t, inner.MaxIdleConnsPerHost, 16)
}

func TestLimitedTransport_HoldsSlotUntilBodyClose(t *testing.T) {
	proceed := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-proceed
		_, _ = io.WriteString(w, "body")
	}))
	defer ts.Close()
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	}()

	lim := newCountingLimiter(0)
	rt := BaseTransport(TransportOptions{Limiter: lim})
	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	assert.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	assert.NoError(t, err)
	assert.Equal(t, lim.outstanding.Load(), int64(1), "the slot must be held once the headers arrive")

	close(proceed)
	b, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.Equal(t, string(b), "body")
	assert.Equal(t, lim.outstanding.Load(), int64(1), "reading the body must not release the slot")

	assert.NoError(t, resp.Body.Close())
	assert.Equal(t, lim.outstanding.Load(), int64(0))
	_ = resp.Body.Close()
	assert.Equal(t, lim.releases.Load(), int64(1), "a double close must release once")
	assert.Equal(t, lim.acquires.Load(), int64(1))
}

func TestLimitedTransport_ReleasesOnTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := ts.URL
	ts.Close()

	lim := newCountingLimiter(1)
	rt := BaseTransport(TransportOptions{Limiter: lim})
	req, err := http.NewRequest(http.MethodGet, addr, nil)
	assert.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	assert.Error(t, err)
	assert.True(t, resp == nil)
	assert.Equal(t, lim.acquires.Load(), int64(1))
	assert.Equal(t, lim.releases.Load(), int64(1))
	assert.Equal(t, lim.outstanding.Load(), int64(0))
}

func TestLimitedTransport_RetryBackoffHoldsNoSlot(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "try again")
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer ts.Close()

	lim := newCountingLimiter(1)
	c, err := NewClient(ts.URL, TransportOptions{Limiter: lim}, 0, nil)
	assert.NoError(t, err)
	var mu sync.Mutex
	var during []int64
	c.httpClient.Transport.(*ExportarrTransport).Backoff = func(int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		during = append(during, lim.outstanding.Load())
		return time.Millisecond
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out struct {
		OK bool `json:"ok"`
	}
	assert.NoError(t, c.DoRequestContext(ctx, "api", &out))
	assert.True(t, out.OK)

	mu.Lock()
	defer mu.Unlock()
	assert.DeepEqual(t, during, []int64{0, 0})
	assert.Equal(t, hits.Load(), int64(3))
	assert.Equal(t, lim.acquires.Load(), int64(3))
	assert.Equal(t, lim.outstanding.Load(), int64(0))
}

func TestLimitedTransport_CancelWhileWaitingForSlot(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()

	lim := newCountingLimiter(1)
	hold, err := lim.Acquire(context.Background())
	assert.NoError(t, err)
	defer hold()

	u, err := url.Parse(ts.URL + "/base?token=hunter2")
	assert.NoError(t, err)
	u.User = url.UserPassword("admin", "hunter2")
	c, err := NewClient(u.String(), TransportOptions{Limiter: lim}, 0, queryKeyAuth{})
	assert.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)
	err = c.DoRequestContext(ctx, "api", nil)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.Contains(t, err.Error(), "failed to execute HTTP Request("+ts.URL+"/base/api)")
	assert.Contains(t, err.Error(), "waiting for an upstream request slot")
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), "admin")
	assert.Equal(t, hits.Load(), int64(0))
	assert.Equal(t, lim.acquires.Load(), int64(1))
}
