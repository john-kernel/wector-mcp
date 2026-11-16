package crawler

import (
	"context"
	"fmt"
	"net/netip"
	neturl "net/url"
	"strings"

	"github.com/john-kernel/wector-mcp/internal/index"
)

// Crawler reads robots.txt and crawls sitemaps to discover page URLs.
type Crawler struct {
	// Fetcher performs HTTP requests. If nil, NewFetcher is used.
	Fetcher *Fetcher
	// MaxSitemaps bounds sitemaps fetched (default 50).
	MaxSitemaps int
	// MaxURLs bounds page URLs collected (default 200000).
	MaxURLs int
	// MaxDepth bounds sitemap index nesting (default 5).
	MaxDepth int
	// Workers bounds how many sitemaps are fetched concurrently (default 8).
	Workers int
	// TrySitemapFallback fetches /sitemap.xml when robots.txt lists none (default true).
	TrySitemapFallback *bool
}

// Result summarizes a crawl.
type Result struct {
	SitemapsFetched int
	URLsDiscovered  int
	URLsInserted    int
	Errors          []string
	// Truncated says why the crawl stopped early; "" if it did not.
	Truncated string
	// Retryable is true when no sitemap could be read due to network/server errors.
	Retryable bool
}

// sitemapResult is the outcome of fetching one sitemap in parallel.
type sitemapResult struct {
	pageURLs []index.URL
	nested   []string
	err      error
}

func (c *Crawler) defaults() {
	if c.Fetcher == nil {
		c.Fetcher = NewFetcher()
	}
	if c.MaxSitemaps <= 0 {
		c.MaxSitemaps = 50
	}
	if c.MaxURLs <= 0 {
		c.MaxURLs = 200000
	}
	if c.MaxDepth <= 0 {
		c.MaxDepth = 5
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
}

func (c *Crawler) tryFallback() bool {
	if c.TrySitemapFallback == nil {
		return true
	}
	return *c.TrySitemapFallback
}

// Crawl fetches robots.txt and sitemaps for domain and stores the page URLs.
func (c *Crawler) Crawl(ctx context.Context, domain string, store *index.SQLiteStore) (Result, error) {
	c.defaults()

	domain = normalizeDomain(domain)
	if domain == "" {
		return Result{}, fmt.Errorf("empty domain")
	}

	var res Result
	// transient is set when a fetch failed in a way that may clear on retry.
	transient := false
	sitemapErrors := 0

	// Strip "www." so subdomains count as the same site.
	site := index.NormalizeSite(domain)
	queueCap := c.MaxSitemaps * 20

	queue := make([]string, 0, c.MaxSitemaps)
	seen := make(map[string]bool)

	// Seed from robots.txt.
	sitemaps, err := c.Fetcher.FetchRobots(ctx, domain)
	if err != nil && Transient(err) {
		transient = true
		res.Errors = append(res.Errors, err.Error())
	}
	for _, s := range sitemaps {
		if s == "" || seen[s] || !sitemapAllowed(site, s) {
			continue
		}
		if len(seen) >= queueCap {
			if res.Truncated == "" {
				res.Truncated = fmt.Sprintf("only the first %d sitemap URLs were considered", queueCap)
			}
			break
		}
		seen[s] = true
		queue = append(queue, s)
	}

	// Fall back to /sitemap.xml when robots.txt lists none.
	if len(queue) == 0 && c.tryFallback() {
		fallback := c.Fetcher.FallbackSitemapURL(domain)
		seen[fallback] = true
		queue = append(queue, fallback)
	}

	var pages []index.URL

	// limitHit records the first truncation reason and reports whether the
	// crawl must stop.
	limitHit := func() bool {
		switch {
		case ctx.Err() != nil:
			if res.Truncated == "" {
				res.Truncated = "the crawl ran out of time"
			}
		case res.SitemapsFetched >= c.MaxSitemaps:
			if res.Truncated == "" {
				res.Truncated = fmt.Sprintf("only the first %d sitemaps were read", c.MaxSitemaps)
			}
		case len(pages) >= c.MaxURLs:
			if res.Truncated == "" {
				res.Truncated = fmt.Sprintf("only the first %d URLs were kept", c.MaxURLs)
			}
		default:
			return false
		}
		return true
	}

	// batchResult pairs a sitemap batch slot with its fetch outcome so
	// results can be merged in queue order regardless of completion order.
	type batchResult struct {
		idx int
		res sitemapResult
	}

crawl:
	for depth := 1; len(queue) > 0; depth++ {
		if depth > c.MaxDepth {
			if res.Truncated == "" {
				res.Truncated = fmt.Sprintf("sitemap indexes nested deeper than %d levels were not read", c.MaxDepth)
			}
			break
		}
		var next []string
		for len(queue) > 0 {
			if limitHit() {
				break crawl
			}

			// Fetch the next batch of sitemaps concurrently. Results are
			// merged in queue order so which sitemaps are read, and the
			// truncation messages, stay deterministic.
			batch := queue
			if len(batch) > c.Workers {
				batch = batch[:c.Workers]
			}
			if rem := c.MaxSitemaps - res.SitemapsFetched; len(batch) > rem {
				batch = batch[:rem]
			}
			queue = queue[len(batch):]

			// Workers report through a buffered channel, so a canceled
			// context can abort the wait immediately: the doomed requests
			// fail as the context cancels, and their results are dropped.
			completions := make(chan batchResult, len(batch))
			for i, sitemapURL := range batch {
				go func(i int, sitemapURL string) {
					pageURLs, nested, err := c.Fetcher.FetchSitemap(ctx, sitemapURL)
					completions <- batchResult{
						idx: i,
						res: sitemapResult{pageURLs: pageURLs, nested: nested, err: err},
					}
				}(i, sitemapURL)
			}

			results := make([]sitemapResult, len(batch))
			for got := 0; got < len(batch); got++ {
				select {
				case br := <-completions:
					results[br.idx] = br.res
				case <-ctx.Done():
					if res.Truncated == "" {
						res.Truncated = "the crawl ran out of time"
					}
					break crawl
				}
			}

			for i := range results {
				if limitHit() {
					break crawl
				}
				r := &results[i]

				if r.err != nil {
					if Transient(r.err) {
						transient = true
					}
					sitemapErrors++
					res.Errors = append(res.Errors, r.err.Error())
					continue
				}
				res.SitemapsFetched++

				for _, page := range r.pageURLs {
					if !sameSite(site, page.Domain) {
						continue
					}
					if len(pages) >= c.MaxURLs {
						if res.Truncated == "" {
							res.Truncated = fmt.Sprintf("only the first %d URLs were kept", c.MaxURLs)
						}
						break crawl
					}
					pages = append(pages, page)
				}

				for _, ns := range r.nested {
					if ns == "" || seen[ns] || !sitemapAllowed(site, ns) {
						continue
					}
					if len(seen) >= queueCap {
						if res.Truncated == "" {
							res.Truncated = fmt.Sprintf("only the first %d sitemap URLs were considered", queueCap)
						}
						break
					}
					seen[ns] = true
					next = append(next, ns)
				}
			}
		}
		queue = next
	}
	if res.Truncated == "" && sitemapErrors > 0 {
		res.Truncated = fmt.Sprintf("%d sitemap(s) could not be read; see the errors list", sitemapErrors)
	}
	if res.Truncated == "" && ctx.Err() != nil {
		res.Truncated = "the crawl ran out of time"
	}

	res.URLsDiscovered = len(pages)

	// Store with a context that outlives the crawl deadline.
	storeCtx := context.WithoutCancel(ctx)

	inserted, err := store.InsertURLs(storeCtx, pages)
	if err != nil {
		return res, err
	}
	res.URLsInserted = inserted

	// Leave unreached sites unrecorded so they retry.
	if res.SitemapsFetched == 0 && (transient || ctx.Err() != nil) {
		res.Retryable = true
		return res, nil
	}

	// Record the crawl even when nothing was found.
	if err := store.MarkSiteCrawled(storeCtx, domain, len(pages)); err != nil {
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

func sitemapAllowed(site, rawURL string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if sameSite(site, host) {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return true
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified() && !ip.IsMulticast()
}
