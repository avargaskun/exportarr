package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
