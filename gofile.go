// gofile.go
//
// Client for the gofile.io API. Inspired by github.com/jkawamoto/go-pixeldrain.
package gofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Defaults for the client. The user agent and salt are what gofile's web
// client currently uses; if downloads start failing with "error-notPremium"
// again, gofile has probably rotated the salt (see WithSalt).
const (
	DefaultAPIBase   = "https://api.gofile.io"
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	DefaultLanguage = "en-US"
	DefaultSalt     = "12af056dacea0b"
	DefaultPageSize = 1000
)

// Client talks to the gofile API.
type Client struct {
	http        *http.Client
	apiBase     string
	serverBase  string
	token       string
	userAgent   string
	language    string
	salt        string
	proxyPrefix string
	pageSize    int
	now         func() time.Time
}

// Option configures a Client.
type Option func(*Client)

// New creates a client with the given options.
func New(options ...Option) *Client {
	c := &Client{
		http:        http.DefaultClient,
		apiBase:     DefaultAPIBase,
		userAgent:   DefaultUserAgent,
		language:    DefaultLanguage,
		salt:        DefaultSalt,
		proxyPrefix: "",
		pageSize:    DefaultPageSize,
		now:         time.Now,
	}
	for _, opt := range options {
		opt(c)
	}
	return c
}

// WithToken sets the account token (API key or guest token).
func WithToken(token string) Option { return func(c *Client) { c.token = token } }

// WithHTTPClient replaces the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithAPIBase overrides the API base URL (default https://api.gofile.io).
func WithAPIBase(s string) Option { return func(c *Client) { c.apiBase = s } }

// WithUserAgent overrides the user agent. It must stay consistent with the
// website token (the server validates the pair).
func WithUserAgent(s string) Option { return func(c *Client) { c.userAgent = s } }

// WithLanguage overrides the X-BL language header value.
func WithLanguage(s string) Option { return func(c *Client) { c.language = s } }

// WithSalt overrides the website-token salt used when gofile rotates it.
func WithSalt(s string) Option { return func(c *Client) { c.salt = s } }

// WithProxyPrefix routes every request through a URL-prefix rewriting proxy:
// the target URL is percent-escaped and appended to the prefix, e.g.
// "https://test.cors.workers.dev/?" . Useful on hosts where gofile is
// unreachable directly. The proxy must forward User-Agent, X-BL,
// X-Website-Token, Authorization and Range headers.
func WithProxyPrefix(prefix string) Option { return func(c *Client) { c.proxyPrefix = prefix } }

// WithPageSize sets the children page size for folder listing (max 1000).
func WithPageSize(n int) Option { return func(c *Client) { c.pageSize = n } }

// WithServerBase overrides the store-server base URL. By default store
// requests go to https://{server}.gofile.io; setting a fixed base is useful
// for tests and for setups where every host must go through one endpoint.
func WithServerBase(s string) Option { return func(c *Client) { c.serverBase = s } }

// storeURL builds a store-server URL such as
// https://{server}.gofile.io/download/web/{id}/{name}.
func (c *Client) storeURL(server, path string) string {
	if c.serverBase != "" {
		return c.serverBase + path
	}
	return serverURL(server, path)
}

// APIError is a gofile-level error ("status" other than "ok").
type APIError struct {
	Status string
}

func (e *APIError) Error() string { return "gofile: " + e.Status }

// Status values worth checking for.
const (
	StatusNotFound    = "error-notFound"
	StatusNotPremium  = "error-notPremium"
	StatusRateLimited = "error-rateLimit"
)

// ErrMD5Mismatch is returned when a downloaded file's MD5 differs from the
// value reported by the API.
type ErrMD5Mismatch struct {
	Expected string
	Got      string
}

func (e *ErrMD5Mismatch) Error() string {
	return fmt.Sprintf("gofile: md5 mismatch (expected %s, got %s)", e.Expected, e.Got)
}

// envelope is the common JSON shape of gofile responses.
type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

// requestURL applies the proxy prefix when configured.
func (c *Client) requestURL(target string) string {
	if c.proxyPrefix == "" {
		return target
	}
	return c.proxyPrefix + url.QueryEscape(target)
}

// setHeaders fills the headers gofile expects. When token is empty the
// Authorization and website-token headers are omitted.
func (c *Client) setHeaders(h http.Header, token string, windowOffset int) {
	h.Set("User-Agent", c.userAgent)
	h.Set("Accept", "*/*")
	h.Set("Origin", "https://gofile.io")
	h.Set("Referer", "https://gofile.io/")
	h.Set("X-BL", c.language)
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
		h.Set("X-Website-Token", c.websiteToken(windowOffset))
	}
}

// do performs one HTTP request and returns its body. HTTP-level failures are
// wrapped with the status code and a body snippet. Idempotent requests
// (method GET, no body) are retried with backoff on transient server-side
// errors (429 and 5xx, including Cloudflare's 522) — common through
// rate-limited public proxies.
func (c *Client) do(ctx context.Context, method, target string, headers http.Header, body io.Reader, length int64) ([]byte, int, error) {
	// Idempotent requests can be retried on transient server-side errors
	// (429 and 5xx, including Cloudflare's 522) — common through
	// rate-limited public proxies. GET without body and DELETE with a
	// seekable body qualify; streaming bodies never do.
	var seekable io.Seeker
	if body != nil {
		seekable, _ = body.(io.Seeker)
	}
	retryable := (body == nil && method == http.MethodGet) ||
		(seekable != nil && method == http.MethodDelete)

	var data []byte
	var code int
	for attempt := 0; ; attempt++ {
		if attempt > 0 && seekable != nil {
			if _, err := seekable.Seek(0, io.SeekStart); err != nil {
				return nil, 0, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.requestURL(target), body)
		if err != nil {
			return nil, 0, err
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if length >= 0 && body != nil {
			req.ContentLength = length
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, 0, err
		}
		data, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		code = resp.StatusCode
		if err != nil {
			return nil, code, fmt.Errorf("gofile: reading response: %w", err)
		}
		if code < 300 || !retryable || attempt >= 2 {
			break
		}
		if code == http.StatusTooManyRequests ||
			code == http.StatusInternalServerError ||
			code == http.StatusBadGateway ||
			code == http.StatusServiceUnavailable ||
			code == http.StatusGatewayTimeout ||
			code == 522 || code == 523 || code == 524 {
			time.Sleep(time.Duration(3*(attempt+1)) * time.Second)
			continue
		}
		break
	}
	if code >= 300 {
		snippet := string(data)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, code, fmt.Errorf("gofile: unexpected HTTP %d from %s: %s",
			code, target, snippet)
	}
	return data, code, nil
}

// apiCall performs a gofile API request and unmarshals the "data" field into
// out (if out is not nil). A response with status "error-notPremium" is
// retried once with the previous website-token window, which covers requests
// issued right after a 4-hour bucket boundary.
func (c *Client) apiCall(ctx context.Context, method, target string, body io.Reader, length int64, contentType string, out any) error {
	makeHeaders := func(offset int) http.Header {
		h := make(http.Header)
		c.setHeaders(h, c.token, offset)
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		return h
	}

	var lastErr error
	for _, offset := range []int{0, -1} {
		var reader io.Reader
		if body != nil && offset == 0 {
			// The body can only be replayed when it is rewindable.
			if b, ok := body.(*bytesReader); ok {
				_ = b.rewind()
				reader = b
			} else {
				reader = body
			}
		} else if body != nil {
			if b, ok := body.(*bytesReader); ok {
				_ = b.rewind()
				reader = b
			} else {
				break // streaming body already consumed on the first attempt
			}
		}
		data, _, err := c.do(ctx, method, target, makeHeaders(offset), reader, length)
		if err != nil {
			return err
		}
		var env envelope
		if jerr := json.Unmarshal(data, &env); jerr != nil {
			return fmt.Errorf("gofile: invalid response: %w", jerr)
		}
		if env.Status == "ok" {
			if out != nil && len(env.Data) > 0 {
				if jerr := json.Unmarshal(env.Data, out); jerr != nil {
					return fmt.Errorf("gofile: invalid data payload: %w", jerr)
				}
			}
			return nil
		}
		lastErr = &APIError{Status: env.Status}
		if env.Status != StatusNotPremium {
			return lastErr
		}
		// try the previous window once
	}
	return lastErr
}

// bytesReader is a rewindable io.Reader used for request bodies that may be
// replayed on retry.
type bytesReader struct {
	data []byte
	pos  int
}

func newBytesReader(data []byte) *bytesReader { return &bytesReader{data: data} }

func (b *bytesReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *bytesReader) rewind() error {
	b.pos = 0
	return nil
}

// fullURL joins the API base and a path starting with "/".
func (c *Client) fullURL(path string) string { return c.apiBase + path }

// serverURL builds a store-server URL such as
// https://{server}.gofile.io/download/web/{id}/{name}.
func serverURL(server, path string) string {
	return "https://" + server + ".gofile.io" + path
}

var errNoToken = errors.New("gofile: this operation requires an account token (API key)")
