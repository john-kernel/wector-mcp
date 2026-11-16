package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	wectorhttp "wector-mcp/http"
	"wector-mcp/utils"
)

const cacheTime = 500

const serverVersion = "v1.0.0"

const instructions = `wector finds pages on a website by searching the URLs listed in the site's sitemaps, so you can go straight to the right page instead of running a web search.

When you know or can guess which site holds the answer, call search_urls with a few distinctive keywords and that site's domain, then fetch the best result. The first search of a site reads its sitemaps (one slow call); later searches of that site are answered locally with no web requests. Only words in the URL are matched, so prefer topic keywords over full questions. If a search returns a hint saying the site has no sitemap URLs, use web search instead.

The index lives for this session only and is discarded when the session ends.`

type app struct {
	store      *utils.SQLiteStore
	newCrawler func() *wectorhttp.Crawler
}

const crawlTimeout = 90 * time.Second

type checkSitestoreOutput struct {
	Populated bool         `json:"populated" jsonschema:"true if the URL store contains at least one entry"`
	URLCount  int          `json:"url_count" jsonschema:"number of URLs in the store"`
	Sites     []utils.Site `json:"sites" jsonschema:"sites that have already been indexed"`
}

// checkSitestore builds the check_sitestore handler bound to the given a.store.
func (a *app) checkSitestore() mcp.ToolHandlerFor[struct{}, checkSitestoreOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, checkSitestoreOutput, error) {
		count, err := a.store.CountURLs(ctx)
		if err != nil {
			return nil, checkSitestoreOutput{}, err
		}
		sites, err := a.store.Sites(ctx)
		if err != nil {
			return nil, checkSitestoreOutput{}, err
		}
		return nil, checkSitestoreOutput{Populated: count > 0, URLCount: count, Sites: sites}, nil
	}
}

type indexSiteInput struct {
	Domain string `json:"domain" jsonschema:"host or URL of the site to index, e.g. docs.python.org"`
}

type indexSiteOutput struct {
	Domain          string   `json:"domain" jsonschema:"the site that was indexed"`
	SitemapsFetched int      `json:"sitemaps_fetched" jsonschema:"number of sitemap documents read"`
	URLsDiscovered  int      `json:"urls_discovered" jsonschema:"number of page URLs found in the sitemaps"`
	URLsInserted    int      `json:"urls_inserted" jsonschema:"number of URLs that were new to the store"`
	Errors          []string `json:"errors" jsonschema:"sitemaps that could not be fetched or parsed"`
}

// crawlSite indexes domain's robots.txt and sitemaps into a.store.
func (a *app) crawlSite(ctx context.Context, domain string) (indexSiteOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, crawlTimeout)
	defer cancel()

	res, err := a.newCrawler().Crawl(ctx, domain, a.store)
	if err != nil {
		return indexSiteOutput{}, fmt.Errorf("index %s: %w", domain, err)
	}
	if res.Errors == nil {
		res.Errors = []string{}
	}
	return indexSiteOutput{
		Domain:          utils.NormalizeSite(domain),
		SitemapsFetched: res.SitemapsFetched,
		URLsDiscovered:  res.URLsDiscovered,
		URLsInserted:    res.URLsInserted,
		Errors:          res.Errors,
	}, nil
}

// indexSite builds the index_site handler bound to the given a.store.
func (a *app) indexSite() mcp.ToolHandlerFor[indexSiteInput, indexSiteOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in indexSiteInput) (*mcp.CallToolResult, indexSiteOutput, error) {
		if utils.NormalizeSite(in.Domain) == "" {
			return nil, indexSiteOutput{}, fmt.Errorf("domain is required: pass a host such as docs.python.org")
		}
		out, err := a.crawlSite(ctx, in.Domain)
		return nil, out, err
	}
}

type searchURLsInput struct {
	Query  string `json:"query,omitempty" jsonschema:"keywords to look for in page URLs; may include a site:example.com operator; leave empty with a domain to list that site's top-level pages"`
	Domain string `json:"domain,omitempty" jsonschema:"restrict results to this site and its subdomains; it is indexed first if it has not been yet"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 10, max 50)"`
}

type searchURLsOutput struct {
	Results []utils.SearchResult `json:"results" jsonschema:"matching URLs, best first"`
	Indexed *indexSiteOutput     `json:"indexed,omitempty" jsonschema:"present when the site had to be indexed to answer this search"`
	Hint    string               `json:"hint,omitempty" jsonschema:"what to try next when there are no results"`
}

// searchURLs builds the search_urls handler bound to the given a.store. When
// the search names a site that has not been crawled yet, it is crawled first
// so a single tool call is enough.
func (a *app) searchURLs() mcp.ToolHandlerFor[searchURLsInput, searchURLsOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in searchURLsInput) (*mcp.CallToolResult, searchURLsOutput, error) {
		var out searchURLsOutput

		site := utils.NormalizeSite(in.Domain)
		if site == "" {
			site = utils.ParseQuery(in.Query).Site
		}
		if site != "" {
			crawled, err := a.store.SiteCrawled(ctx, site)
			if err != nil {
				return nil, out, err
			}
			if !crawled {
				indexed, err := a.crawlSite(ctx, site)
				if err != nil {
					return nil, out, err
				}
				out.Indexed = &indexed
			}
		}

		results, err := a.store.Search(ctx, in.Query, in.Domain, in.Limit)
		if errors.Is(err, utils.ErrEmptyQuery) {
			return nil, out, fmt.Errorf("nothing to search for: pass keywords in query, a domain, or both")
		}
		if err != nil {
			return nil, out, err
		}
		out.Results = results

		if len(results) == 0 {
			switch {
			case site == "":
				out.Hint = "No indexed URL matches. Pass a domain (or site:example.com) to index a site and search it, or call check_sitestore to see what is indexed."
			default:
				// Tell "nothing matched" apart from "nothing is stored".
				any, err := a.store.Search(ctx, "", site, 1)
				if err != nil {
					return nil, out, err
				}
				if len(any) == 0 {
					out.Hint = "No sitemap URLs could be found for " + site + "; fall back to web search."
				} else {
					out.Hint = "No URL on " + site + " matches these keywords; try fewer or different keywords."
				}
			}
		}
		return nil, out, nil
	}
}

// newServer builds the MCP server and registers the tools.
func newServer(a *app) *mcp.Server {
	no := false
	yes := true

	server := mcp.NewServer(
		&mcp.Implementation{Name: "wector", Title: "Wector URL search", Version: serverVersion},
		&mcp.ServerOptions{Instructions: instructions},
	)
	mcp.AddTool(server, &mcp.Tool{
		Name:  "search_urls",
		Title: "Search site URLs",
		Description: "Find pages on a website by keyword, ranked best first, using the URLs listed in the site's sitemaps. " +
			"Use this before a web search when you know which site should have the answer, then fetch the top result directly. " +
			"Pass the site as domain (or site:example.com in the query); a site is indexed automatically the first time it is searched. " +
			"Without a domain, every site indexed so far in this session is searched. " +
			"Matching is on words in the URL only, so use a few distinctive keywords rather than a full question. " +
			"An empty query with a domain lists that site's top-level pages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes},
	}, a.searchURLs())
	mcp.AddTool(server, &mcp.Tool{
		Name:  "index_site",
		Title: "Index a site",
		Description: "Read a site's robots.txt and sitemaps and store its page URLs for search_urls. " +
			"search_urls does this automatically the first time a site is searched; call this only to refresh a site.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes},
	}, a.indexSite())
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_sitestore",
		Title:       "Check the URL store",
		Description: "Report how many URLs the store holds and which sites have been indexed in this session.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no},
	}, a.checkSitestore())
	return server
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run serves one MCP session on stdio and discards the URL store when the
// session ends, whether the client disconnects or the process is signalled.
func run() error {
	sess, err := openSession()
	if err != nil {
		return err
	}
	defer func() {
		if err := sess.end(); err != nil {
			log.Printf("flush url store: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := newServer(&app{
		store:      sess.store,
		newCrawler: func() *wectorhttp.Crawler { return &wectorhttp.Crawler{} },
	})
	err = server.Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil // asked to stop
	}
	return err
}
