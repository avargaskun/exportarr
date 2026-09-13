// Package client provides the shared authenticated HTTP client used by every
// exporter.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultRequestTimeout caps each HTTP request so a hung instance cannot pin
// scrape goroutines forever; large bazarr/lidarr history payloads can take
// tens of seconds, so the default is generous and overridable via config.
const defaultRequestTimeout = 60 * time.Second

// maxResponseBytes caps each decoded response body. The transport decompresses
// gzip transparently, so this bounds the decompressed size.
const maxResponseBytes = 256 << 20

// Client struct is an *Arr client.
type Client struct {
	httpClient   http.Client
	URL          url.URL
	maxBodyBytes int64
	ctx          context.Context
}

// QueryParams holds URL query parameters.
type QueryParams = url.Values

// TransportOptions configures the transport a client sends requests through.
type TransportOptions struct {
	InsecureSkipVerify bool
	// ProxyFromEnvironment honors HTTP(S)_PROXY/NO_PROXY. Off by default: a
	// proxy would see the API key, session cookies and form credentials.
	ProxyFromEnvironment bool
}

// NewClient method initializes a new *Arr client.
func NewClient(baseURL string, opts TransportOptions, timeout time.Duration, auth Authenticator) (*Client, error) {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL(%s): %w", baseURL, err)
	}

	return &Client{
		httpClient: http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout:   timeout,
			Transport: NewExportarrTransport(BaseTransport(opts), auth),
		},
		URL:          *u,
		maxBodyBytes: maxResponseBytes,
	}, nil
}

func (c *Client) unmarshalBody(b io.Reader, target any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// return recovered panic as error
			err = fmt.Errorf("recovered from panic: %s", r)

			log := slog.Default()
			if log.Enabled(context.Background(), slog.LevelDebug) {
				s := new(strings.Builder)
				if _, copyErr := io.Copy(s, b); copyErr != nil {
					log.Error("Failed to copy body to string in recover",
						"error", copyErr, "recover", r)
				}
				log = log.With("body", s.String())
			}
			log.Error("Recovered while unmarshalling response", "error", r)
		}
	}()
	err = json.NewDecoder(b).Decode(target)
	return
}

// WithContext returns a shallow copy of c whose requests, when made without
// an explicit context, are bound to ctx (like http.Request.WithContext). It
// scopes one collection's requests to its deadline.
func (c *Client) WithContext(ctx context.Context) *Client {
	scoped := *c
	scoped.ctx = ctx
	return &scoped
}

// DoRequest - Take a HTTP Request and return Unmarshaled data
func (c *Client) DoRequest(endpoint string, target any, queryParams ...QueryParams) error {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return c.DoRequestContext(ctx, endpoint, target, queryParams...)
}

// DoRequestContext is DoRequest bound to ctx: cancelling ctx aborts the request.
func (c *Client) DoRequestContext(ctx context.Context, endpoint string, target any, queryParams ...QueryParams) error {
	values := c.URL.Query()

	// merge all query params
	for _, m := range queryParams {
		for key, vals := range m {
			for _, val := range vals {
				values.Add(key, val)
			}
		}
	}

	endpointURL := c.URL.JoinPath(endpoint)
	endpointURL.RawQuery = values.Encode()
	logURL := redactURL(endpointURL)
	slog.Debug("Sending HTTP request", "url", logURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP Request(%s): %w", logURL, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error repeats the full URL; the redacted one is already in the message.
		if uerr := (*url.Error)(nil); errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("failed to execute HTTP Request(%s): %w", logURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	err = c.unmarshalBody(http.MaxBytesReader(nil, resp.Body, c.maxBodyBytes), target)
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		return fmt.Errorf("response from %s exceeds %d bytes", logURL, tooLarge.Limit)
	}
	return err
}

// Get fetches an endpoint and decodes the JSON response into T.
func Get[T any](c *Client, endpoint string, queryParams ...QueryParams) (T, error) {
	var out T
	err := c.DoRequest(endpoint, &out, queryParams...)
	return out, err
}

// GetContext is Get bound to ctx.
func GetContext[T any](ctx context.Context, c *Client, endpoint string, queryParams ...QueryParams) (T, error) {
	var out T
	err := c.DoRequestContext(ctx, endpoint, &out, queryParams...)
	return out, err
}

// redactURL renders u as scheme://host[:port]/path, dropping the userinfo,
// query and fragment, any of which can carry credentials.
func redactURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// BaseTransport returns a clone of the default transport configured by opts.
// Cloning keeps these settings scoped to this client instead of mutating the
// process-wide http.DefaultTransport.
func BaseTransport(opts TransportOptions) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Every collector in a command scrapes the same host concurrently; the
	// default of 2 idle conns per host forces constant TLS re-handshakes.
	transport.MaxIdleConnsPerHost = 16
	transport.Proxy = nil
	if opts.ProxyFromEnvironment {
		transport.Proxy = http.ProxyFromEnvironment
	}
	if opts.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via --disable-ssl-verify
	}
	return transport
}
