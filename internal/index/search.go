package index

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
	// DefaultSearchLimit is the number of results returned by default.
	DefaultSearchLimit = 10
	// MaxSearchLimit caps the number of results a single search can return.
	MaxSearchLimit = 50

	// candidatePool is how many FTS matches are pulled for re-ranking.
	candidatePool = 300

	// bm25 column weights for urls_fts (domain, segments, keywords).
	weightDomain   = 1.5
	weightSegments = 1.0
	weightKeywords = 2.0

	// Final score mix.
	mixCoverage = 0.6
	mixBM25     = 0.3
	mixDepth    = 0.1

	// prefixMatchCredit is the coverage for matching only the start of a word.
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

// browseSite lists a site's URLs, shallowest first.
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

// matchExpr builds the FTS5 MATCH expression for terms.
func matchExpr(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		st := stem(term)
		if len([]rune(st)) >= minPrefixLen {
			parts = append(parts, `"`+st+`"*`)
		} else {
			parts = append(parts, `"`+term+`"`)
		}
		if p := iesPlural(term); p != "" {
			parts = append(parts, `"`+p+`"`)
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

	// bm25 is negative; normalize to (0, 1].
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
		// Every candidate matched at least one term.
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

	// Keep only the best-ranked URL of each page (e.g. translations).
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

// stripLocale removes a leading locale segment from path.
func stripLocale(path string) string {
	rest := strings.TrimPrefix(path, "/")
	first, tail, _ := strings.Cut(rest, "/")
	if !isLocale(first) {
		return path
	}
	return "/" + tail
}

// languageCodes holds the ISO 639-1 two-letter language codes.
var languageCodes = func() map[string]struct{} {
	const codes = "aa ab ae af ak am an ar as av ay az ba be bg bh bi bm bn bo br bs " +
		"ca ce ch co cr cs cu cv cy da de dv dz ee el en eo es et eu fa ff fi fj fo fr fy " +
		"ga gd gl gn gu gv ha he hi ho hr ht hu hy hz ia id ie ig ii ik io is it iu ja jv " +
		"ka kg ki kj kk kl km kn ko kr ks ku kv kw ky la lb lg li ln lo lt lu lv mg mh mi " +
		"mk ml mn mr ms mt my na nb nd ne ng nl nn no nr nv ny oc oj om or os pa pi pl ps " +
		"pt qu rm rn ro ru rw sa sc sd se sg si sk sl sm sn so sq sr ss st su sv sw ta te " +
		"tg th ti tk tl tn to tr ts tt tw ty ug uk ur uz ve vi vo wa wo xh yi yo za zh zu"
	m := make(map[string]struct{})
	for _, c := range strings.Fields(codes) {
		m[c] = struct{}{}
	}
	return m
}()

// isLocale reports whether segment is a language code, alone or with a subtag.
func isLocale(segment string) bool {
	s := strings.ToLower(segment)
	isAlpha := func(s string) bool {
		for _, r := range s {
			if r < 'a' || r > 'z' {
				return false
			}
		}
		return true
	}
	switch len(s) {
	case 2:
	case 5, 7:
		if (s[2] != '-' && s[2] != '_') || !isAlpha(s[3:]) {
			return false
		}
	default:
		return false
	}
	_, ok := languageCodes[s[:2]]
	return ok
}

// termCredit scores how well term is represented in tokens.
func termCredit(term string, tokens map[string]struct{}) float64 {
	if _, ok := tokens[term]; ok {
		return 1
	}
	st := stem(term)
	if _, ok := tokens[st]; ok {
		return 1
	}
	if p := iesPlural(term); p != "" {
		if _, ok := tokens[p]; ok {
			return 1
		}
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
