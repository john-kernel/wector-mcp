// Package crawler fetches robots.txt and sitemaps over HTTP(S) and normalizes their URLs.
package crawler

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/john-kernel/wector-mcp/internal/index"
)

const defaultUserAgent = "wector/1.0 (+sitemap fetcher)"

// Fetcher retrieves robots.txt and sitemap documents.
type Fetcher struct {
	// Client performs requests. If nil, a client with a 20s timeout is used.
	Client *nethttp.Client
	// UserAgent is sent with every request. If empty, defaultUserAgent is used.
	UserAgent string
	// MaxBodySize bounds response bodies. If <= 0, no limit is applied.
	MaxBodySize int64
	// Scheme is used to construct URLs when BaseURL is not set (default https).
	Scheme string
	// BaseURL overrides the origin for robots.txt and fallback sitemap URLs.
	BaseURL string
}

// NewFetcher returns a Fetcher with production-safe defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client: &nethttp.Client{
			Timeout: 20 * time.Second,
		},
		UserAgent: defaultUserAgent,
	}
}

// maxBodySize returns the configured body bound, or 0 for no limit.
func (f *Fetcher) maxBodySize() int64 {
	if f.MaxBodySize <= 0 {
		return 0
	}
	return f.MaxBodySize
}

func (f *Fetcher) client() *nethttp.Client {
	if f.Client != nil {
		return f.Client
	}
	return &nethttp.Client{Timeout: 20 * time.Second}
}

func (f *Fetcher) userAgent() string {
	if f.UserAgent != "" {
		return f.UserAgent
	}
	return defaultUserAgent
}

func (f *Fetcher) scheme() string {
	if f.Scheme == "" {
		return "https"
	}
	return f.Scheme
}

// originURL returns the base URL for robots.txt and the fallback sitemap.
func (f *Fetcher) originURL(domain string) *neturl.URL {
	if f.BaseURL != "" {
		if u, err := neturl.Parse(f.BaseURL); err == nil {
			return u
		}
	}
	return &neturl.URL{Scheme: f.scheme(), Host: domain}
}

// FallbackSitemapURL returns the /sitemap.xml URL for a domain.
func (f *Fetcher) FallbackSitemapURL(domain string) string {
	u := f.originURL(domain)
	u.Path = "/sitemap.xml"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// FetchError is returned when a request fails before a complete response arrives.
type FetchError struct {
	URL string
	Err error
}

func (e *FetchError) Error() string { return fmt.Sprintf("GET %s: %v", e.URL, e.Err) }
func (e *FetchError) Unwrap() error { return e.Err }

// StatusError is returned by Get for a non-2xx response.
type StatusError struct {
	URL    string
	Code   int
	Status string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: unexpected status %s", e.URL, e.Status)
}

// Transient reports whether err is likely to clear on a later attempt.
func Transient(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == nethttp.StatusRequestTimeout ||
			se.Code == nethttp.StatusTooManyRequests || se.Code >= 500
	}
	var fe *FetchError
	return errors.As(err, &fe)
}

// Get retrieves a URL, rejects non-2xx responses, bounds size, and decompresses gzip.
func (f *Fetcher) Get(ctx context.Context, rawURL string) ([]byte, error) {
	max := f.maxBodySize()

	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", f.userAgent())
	req.Header.Set("Accept", "application/xml,text/xml,text/plain,*/*")

	resp, err := f.client().Do(req)
	if err != nil {
		return nil, &FetchError{URL: rawURL, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{URL: rawURL, Code: resp.StatusCode, Status: resp.Status}
	}

	body, err := readBody(resp.Body, max)
	if err != nil {
		return nil, &FetchError{URL: rawURL, Err: fmt.Errorf("read body: %w", err)}
	}
	if max > 0 && int64(len(body)) > max {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", rawURL, max)
	}

	// Some servers serve gzipped *.xml.gz without Content-Encoding; sniff the magic bytes.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gunzip %s: %w", rawURL, err)
		}
		defer zr.Close()

		body, err = readBody(zr, max)
		if err != nil {
			return nil, fmt.Errorf("gunzip read %s: %w", rawURL, err)
		}
		if max > 0 && int64(len(body)) > max {
			return nil, fmt.Errorf("GET %s: decompressed body exceeds %d bytes", rawURL, max)
		}
	}

	return body, nil
}

// readBody reads r fully, bounding the result to max bytes when max > 0.
func readBody(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return io.ReadAll(r)
	}
	return io.ReadAll(io.LimitReader(r, max+1))
}

// FetchRobots returns the absolute Sitemap URLs from domain's robots.txt.
func (f *Fetcher) FetchRobots(ctx context.Context, domain string) ([]string, error) {
	base := f.originURL(domain)
	base.Path = "/robots.txt"
	base.RawQuery = ""
	base.Fragment = ""
	body, err := f.Get(ctx, base.String())
	if err != nil {
		return nil, err
	}

	sitemaps := index.ParseRobots(body)
	out := make([]string, 0, len(sitemaps))
	for _, s := range sitemaps {
		if resolved := resolveRef(base, s); resolved != "" {
			out = append(out, resolved)
		}
	}
	return out, nil
}

// FetchSitemap parses a sitemap into normalized page URLs and nested sitemap URLs.
func (f *Fetcher) FetchSitemap(ctx context.Context, rawURL string) ([]index.URL, []string, error) {
	body, err := f.Get(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}

	pageLocs, sitemapLocs, err := index.ParseSitemap(body)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", rawURL, err)
	}

	base, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse sitemap url %q: %w", rawURL, err)
	}

	pages := make([]index.URL, 0, len(pageLocs))
	for _, loc := range pageLocs {
		resolved := resolveRef(base, loc)
		if resolved == "" {
			continue
		}
		page, err := index.ParsePageURL(resolved)
		if err != nil {
			continue
		}
		pages = append(pages, page)
	}

	sitemaps := make([]string, 0, len(sitemapLocs))
	for _, loc := range sitemapLocs {
		if resolved := resolveRef(base, loc); resolved != "" {
			sitemaps = append(sitemaps, resolved)
		}
	}

	return pages, sitemaps, nil
}

func resolveRef(base *neturl.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := neturl.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}
