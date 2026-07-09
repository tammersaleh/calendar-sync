// Package feed fetches iCalendar feed snapshots over HTTP with conditional-GET
// validators and an in-memory cache gate. It does NOT parse iCal (that is
// internal/ical); Fetch returns the raw body bytes.
//
// The feed URL is a bearer secret: anyone holding it can read the user's
// private calendar. This package never logs and never lets the full URL escape
// into a returned error - only the URL's host does. A cross-host redirect guard
// stops a redirect from leaking the secret path or shifting trust to another
// origin.
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tunables for the cache gate. The TTL derived from response headers is clamped
// into [minTTL, maxTTL]; defaultTTL applies when the server advertises neither
// Cache-Control: max-age nor X-PUBLISHED-TTL.
const (
	defaultTTL     = 15 * time.Minute
	minTTL         = 60 * time.Second
	maxTTL         = 24 * time.Hour
	requestTimeout = 30 * time.Second
	maxRedirects   = 10
)

// Fetcher performs conditional GETs against a single feed URL, maintaining
// ETag / Last-Modified validators and a next-fetch cache gate across calls.
// One Fetcher per feed. Not safe for concurrent use (the daemon calls it
// serially within a tick).
type Fetcher struct {
	Client *http.Client     // nil => a default client with a sane timeout
	Now    func() time.Time // nil => time.Now; injectable for tests

	etag         string    // last seen ETag; sent as If-None-Match
	lastModified string    // last seen Last-Modified; sent as If-Modified-Since
	nextFetchAt  time.Time // cache gate: no HTTP call before this instant

	// guardedClient is the *http.Client value the cross-host redirect guard has
	// already been installed on. Compared by pointer identity so client()
	// installs the guard exactly once per client rather than re-wrapping
	// CheckRedirect on every Fetch (which would grow an unbounded closure chain
	// over the daemon's lifetime).
	guardedClient *http.Client
}

// Result is the outcome of a Fetch.
type Result struct {
	Body    []byte // populated only when Changed is true
	Changed bool   // true iff a fresh 200 body was fetched this call
	Skipped bool   // true iff the cache gate short-circuited (no HTTP call made)
	// 304 Not Modified => Changed=false, Skipped=false.
}

// Fetch performs a conditional GET unless the in-memory cache gate says it is
// too soon. rawURL is treated as a secret and never appears in returned errors
// or logs.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error) {
	now := f.now()

	// Cache gate. First call (zero nextFetchAt) always fetches.
	if !f.nextFetchAt.IsZero() && now.Before(f.nextFetchAt) {
		return Result{Skipped: true}, nil
	}

	// Parse before anything else so a malformed URL never reaches the wire and
	// never gets echoed back to the caller.
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return Result{}, errors.New("feed: invalid feed URL")
	}
	host := parsed.Host

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("feed: build request for host %s: %w", host, sanitize(err, rawURL))
	}
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}
	if f.lastModified != "" {
		req.Header.Set("If-Modified-Since", f.lastModified)
	}

	resp, err := f.client().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("feed: request to host %s failed: %w", host, sanitize(err, rawURL))
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Result{}, fmt.Errorf("feed: read body from host %s: %w", host, sanitize(err, rawURL))
		}
		f.etag = resp.Header.Get("ETag")
		f.lastModified = resp.Header.Get("Last-Modified")
		f.nextFetchAt = now.Add(f.ttl(resp))
		return Result{Body: body, Changed: true}, nil
	case http.StatusNotModified:
		f.nextFetchAt = now.Add(f.ttl(resp))
		return Result{}, nil
	default:
		// Do NOT advance the gate: the next tick retries.
		return Result{}, fmt.Errorf("feed: unexpected status %d from host %s", resp.StatusCode, host)
	}
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// client returns the fetcher's HTTP client, constructing a default one when the
// caller left Client nil. In all cases it guarantees the cross-host redirect
// guard is installed: a nil CheckRedirect is set to the guard; an existing one
// is wrapped so the guard always runs first, then delegates. The caller keeps
// same-host redirect policy but can never silently opt out of the secret guard.
func (f *Fetcher) client() *http.Client {
	if f.Client == nil {
		f.Client = &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: crossHostRedirect,
		}
		f.guardedClient = f.Client
		return f.Client
	}
	// Install the guard once per distinct client. Re-wrapping on every call
	// would nest a new closure around the previous one each tick, leaking
	// unbounded closures over the daemon's lifetime.
	if f.guardedClient == f.Client {
		return f.Client
	}
	prev := f.Client.CheckRedirect
	if prev == nil {
		f.Client.CheckRedirect = crossHostRedirect
	} else {
		f.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if err := crossHostRedirect(req, via); err != nil {
				return err
			}
			return prev(req, via)
		}
	}
	f.guardedClient = f.Client
	return f.Client
}

// crossHostRedirect refuses any redirect that changes the host relative to the
// original request, and caps the redirect chain length. Same-host redirects
// (path/query changes) are allowed. via[0] is the original request.
func crossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("feed: stopped after %d redirects", maxRedirects)
	}
	if req.URL.Host != via[0].URL.Host {
		// The target host is not the secret (the secret is the path/query), so
		// naming it here is safe and useful.
		return fmt.Errorf("feed: refusing cross-host redirect to %s", req.URL.Host)
	}
	return nil
}

// ttl derives the cache-gate interval from response headers, clamped into
// [minTTL, maxTTL]. Cache-Control: max-age wins; else X-PUBLISHED-TTL; else the
// default.
func (f *Fetcher) ttl(resp *http.Response) time.Duration {
	if d, ok := maxAge(resp.Header.Get("Cache-Control")); ok {
		return clampTTL(d)
	}
	if d, ok := parseISO8601Duration(resp.Header.Get("X-PUBLISHED-TTL")); ok {
		return clampTTL(d)
	}
	return clampTTL(defaultTTL)
}

func clampTTL(d time.Duration) time.Duration {
	switch {
	case d < minTTL:
		return minTTL
	case d > maxTTL:
		return maxTTL
	default:
		return d
	}
}

// sanitize returns an error whose message cannot contain the secret URL. The
// stdlib wraps transport failures in *url.Error, whose Error() embeds the full
// request URL; returning that verbatim would leak the bearer token. We keep
// only the wrapped cause (net.OpError etc. carry host:port, never the path) and
// belt-and-braces refuse anything still containing the raw URL.
func sanitize(err error, rawURL string) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if rawURL != "" && strings.Contains(err.Error(), rawURL) {
		return errors.New("request failed")
	}
	return err
}
