// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package drapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/version"
)

// Config describes how to reach a DataRobot instance.
type Config struct {
	// Endpoint is the API root and already includes /api/v2, e.g.
	// https://app.datarobot.com/api/v2.
	Endpoint string
	Token    string
	// CACertFile is a PEM bundle to trust in addition to the system roots. Go only honours
	// SSL_CERT_FILE on some platforms, so the CLI's --ca-cert is read explicitly.
	CACertFile string
	// InsecureSkipVerify disables certificate verification. Mirrors `dr -k`.
	InsecureSkipVerify bool
	// RequestTimeout bounds a single request. Zero means the package default.
	RequestTimeout time.Duration
	// MaxRetries bounds retries of a retryable failure. Zero means the package default.
	MaxRetries int
	// UserAgentSuffix is appended to the product token, for tests and diagnostics.
	UserAgentSuffix string
}

// Client talks to the DataRobot API.
type Client struct {
	base      *url.URL
	token     string
	userAgent string
	http      *http.Client
	// blob is used for presigned URLs. It is a distinct client with no credentials because
	// sending an Authorization header alongside a presigned signature returns 400.
	blob       *http.Client
	maxRetries int
}

const (
	defaultRequestTimeout = 120 * time.Second
	// uploadTimeout is generous because a pack body can be tens of megabytes over a slow
	// uplink; 256 MiB at 7 MiB/s is already 35 seconds.
	uploadTimeout     = 30 * time.Minute
	defaultMaxRetries = 5
)

// New builds a client. It fails if the endpoint or token is missing or unusable.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("no DataRobot endpoint configured")
	}

	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("no DataRobot API token configured")
	}

	// Exactly one trailing slash, so url.Parse().JoinPath keeps the /api/v2 prefix instead
	// of treating it as a file and dropping the last segment.
	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("parsing endpoint %q: %w", cfg.Endpoint, err)
	}

	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("endpoint %q must be an http(s) URL", cfg.Endpoint)
	}

	transport, err := newTransport(cfg)
	if err != nil {
		return nil, err
	}

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}

	userAgent := fmt.Sprintf("dr-sync/%s (+https://github.com/datarobot/golang-datarobot-filesync)", version.Short())
	if cfg.UserAgentSuffix != "" {
		userAgent += " " + cfg.UserAgentSuffix
	}

	return &Client{
		base:      base,
		token:     cfg.Token,
		userAgent: userAgent,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A 303 from downloads/ points at S3. Following it here would carry the
			// Authorization header cross-host; dr-sync uses links/ instead and treats any
			// redirect as a bug worth surfacing.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("unexpected redirect to %s: the API client does not follow redirects", req.URL.Host)
			},
		},
		blob: &http.Client{
			Transport: transport,
			// No overall timeout: a blob download is bounded by the caller's context, not
			// by a fixed wall clock, because a pack can legitimately take minutes.
			CheckRedirect: stripAuthorizationOnRedirect,
		},
		maxRetries: maxRetries,
	}, nil
}

// Endpoint returns the configured API root.
func (c *Client) Endpoint() string {
	return strings.TrimRight(c.base.String(), "/")
}

func newTransport(cfg Config) (http.RoundTripper, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}

	transport = transport.Clone()

	if cfg.InsecureSkipVerify {
		// Requested explicitly via -k; the caller has accepted the risk.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in
	}

	if cfg.CACertFile == "" {
		return transport, nil
	}

	pem, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA bundle %s: %w", cfg.CACertFile, err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s contains no usable certificates", cfg.CACertFile)
	}

	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	transport.TLSClientConfig.RootCAs = pool

	return transport, nil
}

// stripAuthorizationOnRedirect removes credentials when a presigned URL redirects. Go
// already drops the header across hosts, but a same-host redirect would keep it and S3
// answers 400 when a presigned signature and an Authorization header both appear.
func stripAuthorizationOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}

	req.Header.Del("Authorization")

	return nil
}

// resolve turns an API-relative path into an absolute URL. Paths must be relative and are
// expected to carry their trailing slash, which the API requires.
func (c *Client) resolve(path string, query url.Values) (string, error) {
	if strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("path %q must be relative to the endpoint", path)
	}

	ref, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parsing path %q: %w", path, err)
	}

	resolved := c.base.ResolveReference(ref)

	if len(query) > 0 {
		resolved.RawQuery = query.Encode()
	}

	return resolved.String(), nil
}

// request is one API call. Body, when set, must be re-readable so a retry can replay it.
type request struct {
	method string
	path   string
	query  url.Values
	// body produces a fresh reader per attempt. Nil means no body.
	body func() (io.ReadCloser, error)
	// contentLength is required whenever body is set: a known length avoids chunked
	// transfer encoding, which some gateways reject for large uploads.
	contentLength int64
	contentType   string
	timeout       time.Duration
	// absoluteURL bypasses path resolution, for following an absolute `next` link.
	absoluteURL string
}

// jsonBody marshals v into a replayable request body.
func jsonBody(v any) (func() (io.ReadCloser, error), int64, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, 0, fmt.Errorf("encoding request body: %w", err)
	}

	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(encoded)), nil
	}, int64(len(encoded)), nil
}

// do performs req with retries and decodes a JSON response into out, which may be nil.
func (c *Client) do(ctx context.Context, req request, out any) error {
	resp, err := c.doRaw(ctx, req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)

		return err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", req.method, req.path, err)
	}

	// A few endpoints answer 204 or an empty 200; an empty body leaves out untouched.
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", req.method, req.path, err)
	}

	return nil
}

// doRaw performs req and returns the successful response with its body unread. The caller
// owns closing it.
func (c *Client) doRaw(ctx context.Context, req request) (*http.Response, error) {
	target := req.absoluteURL

	if target == "" {
		resolved, err := c.resolve(req.path, req.query)
		if err != nil {
			return nil, err
		}

		target = resolved
	}

	attempt := 0

	for {
		resp, retryAfter, err := c.attempt(ctx, req, target)
		if err == nil {
			return resp, nil
		}

		if attempt >= c.maxRetries || !isRetryable(err) {
			return nil, err
		}

		if waitErr := waitBeforeRetry(ctx, attempt, retryAfter); waitErr != nil {
			return nil, waitErr
		}

		attempt++
	}
}

// attempt performs exactly one HTTP round trip. The returned duration is the server's
// Retry-After, when it supplied one.
func (c *Client) attempt(ctx context.Context, req request, target string) (*http.Response, time.Duration, error) {
	ctx, cancel := withTimeout(ctx, req.timeout)

	var (
		body io.ReadCloser
		err  error
	)

	if req.body != nil {
		body, err = req.body()
		if err != nil {
			cancel()

			return nil, 0, err
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, body)
	if err != nil {
		cancel()

		return nil, 0, fmt.Errorf("building %s %s: %w", req.method, req.path, err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("User-Agent", c.userAgent)
	httpReq.Header.Set("Accept", "application/json")

	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}

	if req.body != nil {
		httpReq.ContentLength = req.contentLength
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		cancel()

		return nil, 0, &transportError{Method: req.method, Path: req.path, Err: err}
	}

	if resp.StatusCode < 300 {
		// The body outlives this function, so the timeout must too; cancel travels with it.
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

		return resp, 0, nil
	}

	defer cancel()
	defer resp.Body.Close()

	// Errors are small; reading them in full is what lets us name the failure precisely.
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if readErr != nil {
		errBody = nil
	}

	return nil, retryAfterOf(resp), parseAPIError(req.method, req.path, resp, errBody)
}

// withTimeout applies a per-request timeout, defaulting to none when timeout is zero (the
// client-level timeout still applies).
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, timeout)
}

// cancelOnClose releases a request context when the response body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()

	return c.ReadCloser.Close()
}

// sortStrings keeps error text deterministic without pulling in a dependency.
func sortStrings(values []string) {
	sort.Strings(values)
}
