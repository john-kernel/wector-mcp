# wector

An MCP server that finds pages on a website by searching the URLs listed in the
site's sitemaps, so a model can go straight to the right page instead of
running a web search.

[![Listed on mcpservers.org](https://mcpservers.org/badge.svg)](https://mcpservers.org/servers/john-kernel/wector-mcp.git)

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
camelCase and acronyms are split (`HTMLInputElement` is `html input element`),
and page extensions, stopwords, hashes and long numeric IDs are dropped.

A query term matches a word of the URL when it is, from best to worst:

- the same word, or its plural or singular;
- the same words joined or split differently (`config map` and `configmap`,
  `cronjob` and `cron-jobs`);
- another inflection (`installing`, `installed` and `installation` all match
  `install`);
- the start of a longer word (`auth` matches `authentication`);
- abbreviated in the URL (`parameters` matches `params`);
- misspelled (`controler` matches `controller`), which is only tried for a
  term the site does not have as a word.

Each result's score combines:

- **Coverage (60%)**: how much of the query the URL contains. Terms that are
  rare on the indexed sites count for more than common ones, and a term from
  the site's own name counts for little. A match in the last path segment,
  which names the page, counts in full; one in an earlier segment for 90%, and
  one in the domain for half.
- **Focus (20%)**: how much of the last path segment the query accounts for,
  so `/concepts/ingress` beats `/concepts/ingress-controllers` for `ingress`.
- **Proximity (15%)**: whether terms that are neighbors in the query are
  neighbors in the URL too.
- **Depth (5%)**: how shallow the path is.

Pages that contain every term are considered first; pages missing some terms
only fill in when there are too few of those.

A page published in several languages or versions (`/docs/x`, `/fr/docs/x`,
`/en/5.2/x`, `/en/dev/x`) is one result. The URL returned is the one in your
language (see below), then the version named in the query if there is one
(`modelforms 4.2`), then the newest: `latest`, `stable` or no version at all,
then the highest release number, then `dev` and the like. A version is a path
segment with a dotted or `v`-prefixed number, or one of those names.

A tenth of the score is kept for language: URLs in your language get 0.10
(0.11 when the region matches too, as in `/en-us/` for `en_US`), and URLs in
another language get nothing. URLs without a locale get the same 0.10 if your
language is English and half of it otherwise.

Your locale is read at startup from `LANGUAGE`, `LC_ALL`, `LC_MESSAGES` and
`LANG`, in that order; set `WECTOR_LOCALE` (e.g. `pt_BR`) to override them.
English is assumed when none of them names a language.

## Storage

By default each process uses its own database file under the user cache
directory (`~/.cache/wector/session-<pid>.db` on Linux) and deletes it when the
session ends: when the client disconnects, or on SIGINT or SIGTERM. Files left
behind by a killed process are removed the next time wector starts.

Set `WECTOR_DB` to use a specific file instead. That file is emptied at the
start and end of every session but not deleted, so do not share one path
between sessions that run at the same time.

## Limits

- A crawl reads at most 50 sitemaps and 200,000 URLs per site, considers at
  most 1,000 sitemap locations, and gives up after 90 seconds.
- Sites without a sitemap cannot be indexed.
- Ports are not kept, so sites served on a non-default port are not supported.
- Sitemap locations come from the site being indexed and are fetched as
  given, including ones on other hosts; locations on localhost or on
  loopback, private, link-local, or multicast addresses are skipped.
