package client

import (
	"context"
	"fmt"
	"github.com/onedr0p/exportarr/internal/assert"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/onedr0p/exportarr/internal/arr/config"
	base_client "github.com/onedr0p/exportarr/internal/client"
	"github.com/onedr0p/exportarr/internal/fixtures"
)

var (
	testUser = "testuser1"
	testPass = "hunter2"
	testKey  = "abcdef1234567890abcdef1234567890"
)

type testRoundTripFunc func(req *http.Request) (*http.Response, error)

func (t testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return t(req)
}

func TestRoundTrip_Auth(t *testing.T) {
	parameters := []struct {
		name     string
		auth     base_client.Authenticator
		testFunc func(req *http.Request) (*http.Response, error)
	}{
		{
			name: "APIKey",
			auth: &APIKeyAuth{
				APIKey: testKey,
			},
			testFunc: func(req *http.Request) (*http.Response, error) {
				assert.NotNil(t, req, "Request should not be nil")
				assert.NotNil(t, req.Header, "Request header should not be nil")
				assert.Empty(t, req.Header.Get("Authorization"), "Authorization header should be empty")
				assert.NotEmpty(t, req.Header.Get("X-Api-Key"), "X-Api-Key header should be set")
				assert.Equal(t, req.Header.Get("X-Api-Key"), testKey, "X-Api-Key Header set to wrong value")
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       nil,
					Header:     make(http.Header),
				}, nil
			},
		},
	}
	for _, param := range parameters {
		t.Run(param.name, func(_ *testing.T) {
			transport := base_client.NewExportarrTransport(testRoundTripFunc(param.testFunc), param.auth)
			client := &http.Client{Transport: transport}
			req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			assert.NoError(t, err, "Error creating request: %s", err)
			_, err = client.Do(req)
			assert.NoError(t, err, "Error sending request: %s", err)
		})
	}
}

func TestRoundTrip_FormAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotNil(t, r, "Request should not be nil")
		assert.NotNil(t, r.Header, "Request header should not be nil")
		assert.Empty(t, r.Header.Get("Authorization"), "Authorization header should be empty")
		assert.Equal(t, r.Method, "POST", "Request method should be POST")
		assert.Equal(t, r.URL.Path, "/login", "Request URL should be /login")
		assert.Equal(t, r.Header.Get("Content-Type"), "application/x-www-form-urlencoded", "Content-Type should be application/x-www-form-urlencoded")
		assert.Equal(t, r.FormValue("username"), testUser, "Username should be %s", testUser)
		assert.Equal(t, r.FormValue("password"), testPass, "Password should be %s", testPass)
		http.SetCookie(w, &http.Cookie{
			Name:     "RadarrAuth",
			Value:    "abcdef1234567890abcdef1234567890",
			Expires:  time.Now().Add(24 * time.Hour),
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("OK"))
	}))
	defer ts.Close()
	tsURL, _ := url.Parse(ts.URL)
	auth := &FormAuth{
		Username:    testUser,
		Password:    testPass,
		APIKey:      testKey,
		AuthBaseURL: tsURL,
		Transport:   http.DefaultTransport,
	}
	transport := base_client.NewExportarrTransport(testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		assert.NotNil(t, req, "Request should not be nil")
		assert.NotNil(t, req.Header, "Request header should not be nil")
		cookie, err := req.Cookie("RadarrAuth")
		assert.NoError(t, err, "Cookie should be set")
		assert.Equal(t, "abcdef1234567890abcdef1234567890", cookie.Value, "Cookie should be set")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       nil,
			Header:     make(http.Header),
		}, nil
	}), auth)
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	assert.NoError(t, err, "Error creating request: %s", err)
	_, err = client.Do(req)
	assert.NoError(t, err, "Error sending request: %s", err)
}

func TestRoundTrip_FormAuthFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/?loginFailed=true", http.StatusFound)
	}))
	u, _ := url.Parse(ts.URL)
	auth := &FormAuth{
		Username:    testUser,
		Password:    testPass,
		APIKey:      testKey,
		AuthBaseURL: u,
		Transport:   http.DefaultTransport,
	}
	transport := base_client.NewExportarrTransport(testRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       nil,
			Header:     make(http.Header),
		}, nil
	}), auth)
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	assert.NoError(t, err, "Error creating request: %s", err)
	assert.NotPanics(t, func() {
		_, err = client.Do(req)
	}, "Form Auth should not panic on auth failure")
	assert.Error(t, err, "Form Auth Transport should throw an error when auth fails")
}

func TestRoundTrip_Retries(t *testing.T) {
	parameters := []struct {
		name     string
		testFunc func(req *http.Request) (*http.Response, error)
	}{
		{
			name: "500",
			testFunc: func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       nil,
					Header:     make(http.Header),
				}, nil
			},
		},
		{
			name: "Err",
			testFunc: func(_ *http.Request) (*http.Response, error) {
				return nil, http.ErrNotSupported
			},
		},
	}
	for _, param := range parameters {
		t.Run(param.name, func(t *testing.T) {
			auth := &APIKeyAuth{
				APIKey: testKey,
			}
			attempts := 0
			transport := base_client.NewExportarrTransport(testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts++
				return param.testFunc(req)
			}), auth)
			transport.Backoff = func(int) time.Duration { return 0 }
			client := &http.Client{Transport: transport}
			req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			assert.NoError(t, err, "Error creating request: %s", err)
			_, err = client.Do(req)
			assert.Error(t, err, "Error should be returned from Do()")
			assert.Equal(t, attempts, 3, "Should retry 3 times")
		})
	}
}

func TestRoundTrip_StatusCodes(t *testing.T) {
	parameters := []int{200, 201, 202, 204, 301, 302, 400, 401, 403, 404, 500, 503}
	for _, param := range parameters {
		t.Run(fmt.Sprintf("%d", param), func(t *testing.T) {
			auth := &APIKeyAuth{
				APIKey: testKey,
			}
			transport := base_client.NewExportarrTransport(testRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: param,
					Body:       nil,
					Header:     make(http.Header),
				}, nil
			}), auth)
			transport.Backoff = func(int) time.Duration { return 0 }
			client := &http.Client{Transport: transport}
			req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			assert.Nil(t, err, "Error creating request: %s", err)
			_, err = client.Do(req)
			if param >= 200 && param < 300 {
				assert.NoError(t, err, "Should Not error on 2XX: %s", err)
			} else {
				assert.Error(t, err, "Should error on non-2XX")
			}
		})
	}
}

func TestNewAuth_FormAuthIgnoresEnvironmentProxy(t *testing.T) {
	auth, err := NewAuth(&config.ArrConfig{
		URL:          "http://localhost",
		FormAuth:     true,
		AuthUsername: "user",
		AuthPassword: "pass",
	})
	assert.NoError(t, err)
	transport := auth.(*FormAuth).Transport.(*http.Transport)
	assert.True(t, transport.Proxy == nil, "form auth must not send credentials through an environment proxy")
}

func TestTransportOptions_CopiesLimiter(t *testing.T) {
	lim := fixtures.NewCountingLimiter(0)
	opts := transportOptions(&config.ArrConfig{DisableSSLVerify: true, ProxyFromEnv: true, UpstreamLimiter: lim})
	assert.DeepEqual(t, opts, base_client.TransportOptions{InsecureSkipVerify: true, ProxyFromEnvironment: true, Limiter: lim})

	opts = transportOptions(&config.ArrConfig{})
	assert.True(t, opts.Limiter == nil, "no limiter must stay nil (unlimited)")
}

var formAuthCreds = &fixtures.FormAuthCreds{Username: "admin", Password: "s3cret"}

func newLimitedFormAuthClient(t *testing.T, fake *fixtures.FakeApp, lim base_client.Limiter) *Client {
	t.Helper()
	c, err := NewClient(&config.ArrConfig{
		App: "radarr", APIVersion: "v3", URL: fake.URL, APIKey: fixtures.APIKey,
		FormAuth: true, AuthUsername: formAuthCreds.Username, AuthPassword: formAuthCreds.Password,
		RequestTimeout: 10 * time.Second, UpstreamLimiter: lim,
	})
	assert.NoError(t, err)
	return c
}

func TestFormAuth_LoginPassesThroughLimiter(t *testing.T) {
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "radarr", APIKey: fixtures.APIKey, FormAuth: formAuthCreds})
	lim := fixtures.NewCountingLimiter(0)
	c := newLimitedFormAuthClient(t, fake, lim)

	_, err := Get[map[string]any](c, "system/status")
	assert.NoError(t, err)
	assert.Equal(t, fake.Logins(), 1)
	assert.Equal(t, lim.Acquires(), int64(2), "login and API request must each take a slot")
	assert.Equal(t, lim.Outstanding(), int64(0))

	_, err = Get[map[string]any](c, "system/status")
	assert.NoError(t, err)
	assert.Equal(t, fake.Logins(), 1)
	assert.Equal(t, lim.Acquires(), int64(3))
	assert.Equal(t, lim.Outstanding(), int64(0))
}

func TestFormAuth_SingleSlotDoesNotDeadlock(t *testing.T) {
	const workers = 8
	fake := fixtures.NewFakeApp(t, fixtures.FakeAppOptions{
		App: "radarr", APIKey: fixtures.APIKey, FormAuth: formAuthCreds,
		Behavior: func(*http.Request) fixtures.Action { return fixtures.Delay(10 * time.Millisecond) },
	})
	lim := fixtures.NewCountingLimiter(1)
	c := newLimitedFormAuthClient(t, fake, lim)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			_, err := GetContext[map[string]any](ctx, c, "system/status")
			errs <- err
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	close(start)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("requests through a one-slot limiter did not finish within 10s")
	}
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, fake.Logins(), 1)
	assert.Equal(t, lim.Acquires(), int64(workers+1))
	assert.Equal(t, lim.Peak(), int64(1))
	assert.Equal(t, lim.Outstanding(), int64(0))
	assert.Equal(t, fake.PeakInFlight(), int64(1))
}

func TestFormAuth_FailedLoginReleasesSlot(t *testing.T) {
	loginServer := func(t *testing.T, h http.HandlerFunc) string {
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return ts.URL
	}
	for _, tc := range []struct {
		name     string
		upstream func(t *testing.T) string
		wantErr  string
	}{
		{"wrong password", func(t *testing.T) string {
			return fixtures.NewFakeApp(t, fixtures.FakeAppOptions{App: "radarr", APIKey: fixtures.APIKey, FormAuth: formAuthCreds}).URL
		}, "Login Failed"},
		{"non-302 login response", func(t *testing.T) string {
			return loginServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			})
		}, "Received Status Code 500"},
		{"302 without an arrAuth cookie", func(t *testing.T) string {
			return loginServer(t, func(w http.ResponseWriter, _ *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "unrelated", Value: "x", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
				w.Header().Set("Location", "/")
				w.WriteHeader(http.StatusFound)
			})
		}, "No Cookie with suffix 'arrAuth' found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lim := fixtures.NewCountingLimiter(1)
			c, err := NewClient(&config.ArrConfig{
				App: "radarr", APIVersion: "v3", URL: tc.upstream(t), APIKey: fixtures.APIKey,
				FormAuth: true, AuthUsername: formAuthCreds.Username, AuthPassword: "not-" + formAuthCreds.Password,
				RequestTimeout: 10 * time.Second, UpstreamLimiter: lim,
			})
			assert.NoError(t, err)

			_, err = Get[map[string]any](c, "system/status")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, lim.Acquires(), int64(1), "only the login must take a slot")
			assert.Equal(t, lim.Outstanding(), int64(0))

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := lim.Acquire(ctx)
			assert.NoError(t, err, "the failed login's slot was never released")
			release()
		})
	}
}
