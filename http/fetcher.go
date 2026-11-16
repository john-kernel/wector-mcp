// Package http fetches robots.txt and sitemap documents over HTTP(S) and
// turns their contents into normalized URLs suitable for the SQLite store.
package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	nethttp "net/http"
	neturl "net/url"
	"strings"
	"time"

	"wector-mcp/utils"
)

const defaultUserAgent = "wector/1.0 (+sitemap fetcher)"

// Fetcher is a small, bounded HTTP client for retrieving robots.txt and
// sitemap documents.
type Fetcher struct {
	// Client performs requests. If nil, a client with a 20s timeout is used.
	Client *nethttp.Client
	// UserAgent is sent with every request. If empty, defaultUserAgent is used.
	UserAgent string
	// MaxBodySize bounds both compressed and decompressed response bodies.
	// If <= 0, 10 MiB is used.
	MaxBodySize int64
	// Scheme is used to construct robots.txt and fallback sitemap URLs when
	// BaseURL is not set. If empty, "https" is used.
	Scheme string
	// BaseURL, when set, overrides the origin used for robots.txt and fallback
	// sitemap URLs. It is useful for tests and proxies.
	BaseURL string
}

// NewFetcher returns a Fetcher with production-safe defaults.
func NewFetcher() *Fetcher {
	return &Fetcher{
		Client: &nethttp.Client{
			Timeout: 20 * time.Second,
		},
		UserAgent:   defaultUserAgent,
		MaxBodySize: 10 << 20,
	}
}

func (f *Fetcher) maxBodySize() int64 {
	if f.MaxBodySize <= 0 {
		return 10 << 20
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

// originURL returns the base URL used to locate robots.txt and the fallback
// sitemap for a domain, honoring BaseURL when configured.
func (f *Fetcher) originURL(domain string) *neturl.URL {
	if f.BaseURL != "" {
		if u, err := neturl.Parse(f.BaseURL); err == nil {
			return u
		}
	}
	return &neturl.URL{Scheme: f.scheme(), Host: domain}
}

// FallbackSitemapURL returns the conventional sitemap URL for a domain,
// honoring any BaseURL override.
func (f *Fetcher) FallbackSitemapURL(domain string) string {
	u := f.originURL(domain)
	u.Path = "/sitemap.xml"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Get retrieves a URL and returns its response body. It follows redirects,
// rejects non-2xx responses, bounds the body size, and transparently
// decompresses gzip data whether it was announced via Content-Encoding or
// only detectable from the gzip magic bytes.
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
		return nil, fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: unexpected status %s", rawURL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", rawURL, max)
	}

	// net/http already decompresses Content-Encoding: gzip, but some servers
	// serve *.xml.gz files without that header. Sniff for the gzip magic bytes.
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gunzip %s: %w", rawURL, err)
		}
		defer zr.Close()

		body, err = io.ReadAll(io.LimitReader(zr, max+1))
		if err != nil {
			return nil, fmt.Errorf("gunzip read %s: %w", rawURL, err)
		}
		if int64(len(body)) > max {
			return nil, fmt.Errorf("GET %s: decompressed body exceeds %d bytes", rawURL, max)
		}
	}

	return body, nil
}

// FetchRobots retrieves the robots.txt for domain and returns the Sitemap
// directives it contains, resolved to absolute URLs. The request origin honors
// BaseURL when set, otherwise uses Scheme (default https) and domain.
func (f *Fetcher) FetchRobots(ctx context.Context, domain string) ([]string, error) {
	base := f.originURL(domain)
	base.Path = "/robots.txt"
	base.RawQuery = ""
	base.Fragment = ""
	body, err := f.Get(ctx, base.String())
	if err != nil {
		return nil, err
	}

	sitemaps := utils.ParseRobots(body)
	out := make([]string, 0, len(sitemaps))
	for _, s := range sitemaps {
		if resolved := resolveRef(base, s); resolved != "" {
			out = append(out, resolved)
		}
	}
	return out, nil
}

// FetchSitemap retrieves and parses a sitemap URL. It returns normalized page
// URLs and absolute nested sitemap URLs. Relative loc values are resolved
// against the sitemap URL itself.
func (f *Fetcher) FetchSitemap(ctx context.Context, rawURL string) ([]utils.URL, []string, error) {
	body, err := f.Get(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}

	pageLocs, sitemapLocs, err := utils.ParseSitemap(body)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", rawURL, err)
	}

	base, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse sitemap url %q: %w", rawURL, err)
	}

	pages := make([]utils.URL, 0, len(pageLocs))
	for _, loc := range pageLocs {
		resolved := resolveRef(base, loc)
		if resolved == "" {
			continue
		}
		page, err := utils.ParsePageURL(resolved)
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
