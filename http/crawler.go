package http

import (
	"context"
	"fmt"
	neturl "net/url"
	"strings"

	"wector-mcp/utils"
)

// Crawler discovers page URLs for a domain by reading robots.txt and then
// breadth-first crawling sitemap (and nested sitemap) documents.
type Crawler struct {
	// Fetcher performs HTTP requests. If nil, NewFetcher is used.
	Fetcher *Fetcher
	// MaxSitemaps bounds the total number of sitemap documents fetched.
	// If <= 0, 50 is used.
	MaxSitemaps int
	// MaxURLs bounds the number of page URLs collected before storing.
	// If <= 0, 10000 is used.
	MaxURLs int
	// MaxDepth bounds the nesting depth of sitemap indexes. If <= 0, 5 is used.
	MaxDepth int
	// TrySitemapFallback fetches the conventional /sitemap.xml location when
	// robots.txt lists no sitemaps. Defaults to true when the Crawler zero
	// value is used.
	TrySitemapFallback *bool
}

// Result summarizes a crawl.
type Result struct {
	SitemapsFetched int
	URLsDiscovered  int
	URLsInserted    int
	Errors          []string
}

func (c *Crawler) defaults() {
	if c.Fetcher == nil {
		c.Fetcher = NewFetcher()
	}
	if c.MaxSitemaps <= 0 {
		c.MaxSitemaps = 50
	}
	if c.MaxURLs <= 0 {
		c.MaxURLs = 10000
	}
	if c.MaxDepth <= 0 {
		c.MaxDepth = 5
	}
}

func (c *Crawler) tryFallback() bool {
	if c.TrySitemapFallback == nil {
		return true
	}
	return *c.TrySitemapFallback
}

// Crawl fetches robots.txt and sitemap documents for domain, normalizes the
// discovered page URLs, and stores them via store.InsertURLs.
//
// domain may be given as a bare host or as an http(s) URL; the scheme and any
// trailing path are ignored. Only page URLs on domain or one of its subdomains
// are stored.
func (c *Crawler) Crawl(ctx context.Context, domain string, store *utils.SQLiteStore) (Result, error) {
	c.defaults()

	domain = normalizeDomain(domain)
	if domain == "" {
		return Result{}, fmt.Errorf("empty domain")
	}

	var res Result

	queue := make([]string, 0, c.MaxSitemaps)
	seen := make(map[string]bool)

	// Seed the crawl from robots.txt.
	if sitemaps, err := c.Fetcher.FetchRobots(ctx, domain); err == nil {
		for _, s := range sitemaps {
			if s != "" && !seen[s] {
				seen[s] = true
				queue = append(queue, s)
			}
		}
	}

	// Fall back to the conventional sitemap location when robots.txt does not
	// point at any sitemaps (or could not be fetched).
	if len(queue) == 0 && c.tryFallback() {
		fallback := c.Fetcher.FallbackSitemapURL(domain)
		seen[fallback] = true
		queue = append(queue, fallback)
	}

	// Compare hosts without a leading "www." so that crawling www.example.com
	// keeps URLs on example.com and its other subdomains.
	site := utils.NormalizeSite(domain)

	var pages []utils.URL
	for depth := 1; len(queue) > 0 && depth <= c.MaxDepth; depth++ {
		var next []string
		for _, sitemapURL := range queue {
			if res.SitemapsFetched >= c.MaxSitemaps || len(pages) >= c.MaxURLs {
				break
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}

			pageURLs, nested, err := c.Fetcher.FetchSitemap(ctx, sitemapURL)
			if err != nil {
				res.Errors = append(res.Errors, err.Error())
				continue
			}
			res.SitemapsFetched++

			for _, page := range pageURLs {
				if !sameSite(site, page.Domain) {
					continue
				}
				pages = append(pages, page)
				if len(pages) >= c.MaxURLs {
					break
				}
			}

			for _, ns := range nested {
				if ns != "" && !seen[ns] {
					seen[ns] = true
					next = append(next, ns)
				}
			}
		}
		queue = next
	}

	res.URLsDiscovered = len(pages)

	inserted, err := store.InsertURLs(ctx, pages)
	if err != nil {
		return res, err
	}
	res.URLsInserted = inserted

	// Record the crawl even when it found nothing, so a site without a
	// sitemap is not fetched again on every search.
	if err := store.MarkSiteCrawled(ctx, domain, len(pages)); err != nil {
		return res, err
	}
	return res, nil
}

func normalizeDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if !strings.Contains(domain, "://") {
		domain = "https://" + domain
	}
	u, err := neturl.Parse(domain)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// sameSite reports whether candidate equals target or is a subdomain of it.
func sameSite(target, candidate string) bool {
	if candidate == target {
		return true
	}
	return strings.HasSuffix(candidate, "."+target)
}
