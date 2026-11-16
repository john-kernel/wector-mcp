package utils

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
)

const (
	// DefaultSearchLimit is the number of results returned when the caller
	// does not ask for a specific amount.
	DefaultSearchLimit = 10
	// MaxSearchLimit caps the number of results a single search can return.
	MaxSearchLimit = 50

	// candidatePool is how many FTS matches are pulled for re-ranking.
	candidatePool = 300

	// bm25 column weights for urls_fts (domain, segments, keywords). Curated
	// keywords count most; raw segments still match joined forms such as
	// "javascript" that keyword extraction splits apart.
	weightDomain   = 1.5
	weightSegments = 1.0
	weightKeywords = 2.0

	// Final score mix. Coverage (how many query terms a URL matches)
	// dominates, bm25 separates URLs with equal coverage, and shallow paths
	// get a small boost because they tend to be the canonical page on a topic.
	mixCoverage = 0.6
	mixBM25     = 0.3
	mixDepth    = 0.1

	// prefixMatchCredit is the coverage a term earns for matching only the
	// start of a word ("doc" in "docs") instead of the whole word.
	prefixMatchCredit = 0.7
	// minPrefixLen is the shortest term that is allowed to prefix-match.
	minPrefixLen = 3
)

// ErrEmptyQuery is returned when a search has neither terms nor a site.
var ErrEmptyQuery = errors.New("query has no search terms")

// SearchResult is one ranked URL.
type SearchResult struct {
	URL     string   `json:"url" jsonschema:"the page URL"`
	Score   float64  `json:"score" jsonschema:"relevance between 0 and 1, higher is better"`
	Matched []string `json:"matched" jsonschema:"query terms found in the URL"`
}

type candidate struct {
	scheme, domain, path, segments, keywords string
	bm25                                     float64
}

// Search returns the stored URLs that best match query, best first.
//
// query is free text and may contain a "site:example.com" operator; domain,
// when non-empty, restricts results the same way and takes precedence. A
// site restriction covers the host and its subdomains. With a site but no
// search terms, the site's shallowest URLs are returned.
func (s *SQLiteStore) Search(ctx context.Context, query, domain string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	q := ParseQuery(query)
	site := q.Site
	if d := NormalizeSite(domain); d != "" {
		site = d
	}

	if len(q.Terms) == 0 {
		if site == "" {
			return nil, ErrEmptyQuery
		}
		return s.browseSite(ctx, site, limit)
	}

	sqlText := `
		SELECT u.scheme, u.domain, u.path, u.segments, u.keywords,
		       bm25(urls_fts, ?, ?, ?) AS rank
		FROM urls_fts
		JOIN urls u ON u.id = urls_fts.rowid
		WHERE urls_fts MATCH ?`
	args := []any{weightDomain, weightSegments, weightKeywords, matchExpr(q.Terms)}
	if site != "" {
		sqlText += ` AND (u.domain = ? OR u.domain LIKE '%.' || ?)`
		args = append(args, site, site)
	}
	sqlText += ` ORDER BY rank LIMIT ?`
	args = append(args, candidatePool)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("search urls: %w", err)
	}
	defer rows.Close()

	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.scheme, &c.domain, &c.path, &c.segments, &c.keywords, &c.bm25); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search urls: %w", err)
	}

	results := rank(q.Terms, candidates)
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// browseSite lists a site's URLs, shallowest first, for queries that name a
// site but no search terms.
func (s *SQLiteStore) browseSite(ctx context.Context, site string, limit int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scheme, domain, path FROM urls
		WHERE domain = ?1 OR domain LIKE '%.' || ?1
		ORDER BY length(path) - length(replace(path, '/', '')), length(path), domain, path
		LIMIT ?2`, site, limit)
	if err != nil {
		return nil, fmt.Errorf("browse site %s: %w", site, err)
	}
	defer rows.Close()

	results := []SearchResult{}
	for rows.Next() {
		var scheme, domain, path string
		if err := rows.Scan(&scheme, &domain, &path); err != nil {
			return nil, fmt.Errorf("scan site url: %w", err)
		}
		results = append(results, SearchResult{
			URL:     buildURL(scheme, domain, path),
			Score:   round3(1 / float64(1+pathDepth(path))),
			Matched: []string{},
		})
	}
	return results, rows.Err()
}

// matchExpr builds the FTS5 MATCH expression for terms: any term may match
// (ranking rewards matching more of them), and terms long enough are matched
// as prefixes of their stem so plurals and longer word forms are found.
// Terms contain only letters and digits, so quoting them is always safe.
func matchExpr(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		st := stem(term)
		if len([]rune(st)) >= minPrefixLen {
			parts = append(parts, `"`+st+`"*`)
		} else {
			parts = append(parts, `"`+term+`"`)
		}
	}
	return strings.Join(parts, " OR ")
}

// rank scores candidates against the query terms and orders them best first.
func rank(terms []string, candidates []candidate) []SearchResult {
	results := make([]SearchResult, 0, len(candidates))
	if len(candidates) == 0 {
		return results
	}

	type scoredResult struct {
		SearchResult
		pageKey string
	}
	scored := make([]scoredResult, 0, len(candidates))

	// bm25 is negative, more negative being better; normalize to (0, 1].
	best := 0.0
	for _, c := range candidates {
		best = math.Max(best, -c.bm25)
	}

	for _, c := range candidates {
		tokens := make(map[string]struct{})
		for _, t := range tokenize(c.domain+" "+c.segments+" "+c.keywords, false) {
			tokens[t] = struct{}{}
		}

		credit := 0.0
		matched := []string{}
		for _, term := range terms {
			if m := termCredit(term, tokens); m > 0 {
				credit += m
				matched = append(matched, term)
			}
		}
		// FTS folds diacritics and may match where the comparison above does
		// not; every candidate matched at least one term.
		if credit == 0 {
			credit = prefixMatchCredit
		}

		bm := 1.0
		if best > 0 {
			bm = -c.bm25 / best
		}
		score := mixCoverage*credit/float64(len(terms)) +
			mixBM25*bm +
			mixDepth/float64(1+pathDepth(c.path))

		scored = append(scored, scoredResult{
			SearchResult: SearchResult{
				URL:     buildURL(c.scheme, c.domain, c.path),
				Score:   round3(score),
				Matched: matched,
			},
			pageKey: c.domain + stripLocale(c.path),
		})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if len(a.URL) != len(b.URL) {
			return len(a.URL) < len(b.URL)
		}
		return a.URL < b.URL
	})

	// Keep only the best-ranked URL of each page, so translations of one
	// page (/docs/x, /fr/docs/x, /ja/docs/x) do not crowd out other pages.
	seen := make(map[string]struct{}, len(scored))
	for _, r := range scored {
		if _, ok := seen[r.pageKey]; ok {
			continue
		}
		seen[r.pageKey] = struct{}{}
		results = append(results, r.SearchResult)
	}
	return results
}

// stripLocale removes a leading locale segment such as /fr, /pt-br or /zh_CN
// from path. Paths that differ only in that segment are treated as
// translations of the same page.
func stripLocale(path string) string {
	rest := strings.TrimPrefix(path, "/")
	first, tail, _ := strings.Cut(rest, "/")
	if !isLocale(first) {
		return path
	}
	return "/" + tail
}

func isLocale(segment string) bool {
	isAlpha := func(s string) bool {
		for _, r := range s {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return false
			}
		}
		return true
	}
	switch len(segment) {
	case 2:
		return isAlpha(segment)
	case 5:
		return isAlpha(segment[:2]) && (segment[2] == '-' || segment[2] == '_') && isAlpha(segment[3:])
	}
	return false
}

// termCredit returns how well term is represented in tokens: 1 for a whole
// word match, prefixMatchCredit for matching the start of a word, 0 for none.
func termCredit(term string, tokens map[string]struct{}) float64 {
	if _, ok := tokens[term]; ok {
		return 1
	}
	st := stem(term)
	if _, ok := tokens[st]; ok {
		return 1
	}
	if len([]rune(st)) < minPrefixLen {
		return 0
	}
	for token := range tokens {
		if strings.HasPrefix(token, st) {
			return prefixMatchCredit
		}
	}
	return 0
}

func pathDepth(path string) int {
	return len(strings.FieldsFunc(path, func(r rune) bool { return r == '/' }))
}

func buildURL(scheme, domain, path string) string {
	u := url.URL{Scheme: schemeOrDefault(scheme), Host: domain, Path: path}
	return u.String()
}

func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}
