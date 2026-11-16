# wector

An MCP server that finds pages on a website by searching the URLs listed in the
site's sitemaps, so a model can go straight to the right page instead of
running a web search.

The first search of a site reads its `robots.txt` and sitemaps and stores the
page URLs in an embedded SQLite database. Later searches of that site are
answered locally, with no web requests. The database belongs to one session
and is discarded when the session ends.

## Build

Requires Go 1.26 or newer. No C toolchain is needed.

```sh
go build -o wector ./cmd/wector
```

## Configure

wector speaks MCP over stdio. Register the binary with your client, for
example in Claude Code:

```sh
claude mcp add wector /absolute/path/to/wector
```

or in a JSON client configuration:

```json
{
  "mcpServers": {
    "wector": { "command": "/absolute/path/to/wector" }
  }
}
```

## Tools

| Tool | Arguments | What it does |
|---|---|---|
| `search_urls` | `query`, `domain`, `limit` (all optional, at least one of `query`/`domain`) | Returns the best-matching URLs, best first, each with a 0–1 `score` and the `matched` query terms. A `domain` (or `site:example.com` in the query) restricts results to that site and its subdomains, and indexes the site first if it has not been yet. With only a `domain`, lists the site's top-level pages. |
| `index_site` | `domain` | Reads a site's `robots.txt` and sitemaps and stores its URLs. Only needed to refresh a site; `search_urls` indexes on demand. |
| `check_sitestore` | none | Reports how many URLs are stored and which sites have been indexed. |

When a search finds nothing, the result carries a `hint` saying whether the
site has no sitemap (fall back to web search) or the keywords did not match.

## How results are ranked

Only the words in a URL are searched. Keywords are extracted from each path:
camelCase is split, and page extensions, stopwords, hashes and long numeric IDs
are dropped. A query matches whole words and word prefixes, and plurals match
singulars.

Each result's score combines how many of the query terms the URL contains
(60%), SQLite FTS5 bm25 relevance (30%) and how shallow the path is (10%).
Translations of the same page (`/docs/x`, `/fr/docs/x`) are collapsed into the
best-ranked one.

## Storage

By default each process uses its own database file under the user cache
directory (`~/.cache/wector/session-<pid>.db` on Linux) and deletes it when the
session ends: when the client disconnects, or on SIGINT or SIGTERM. Files left
behind by a killed process are removed the next time wector starts.

Set `WECTOR_DB` to use a specific file instead. That file is emptied at the
start and end of every session but not deleted, so do not share one path
between sessions that run at the same time.

## Limits

- A crawl reads at most 50 sitemaps and 10,000 URLs per site, considers at
  most 1,000 sitemap locations, and gives up after 90 seconds.
- Sites without a sitemap cannot be indexed.
- Ports are not kept, so sites served on a non-default port are not supported.
- Sitemap locations come from the site being indexed and are fetched as
  given, including ones on other hosts; locations on localhost or on
  loopback, private, link-local, or multicast addresses are skipped.
