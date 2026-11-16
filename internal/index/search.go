package index

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultSearchLimit is the number of results returned by default.
	DefaultSearchLimit = 10
	// MaxSearchLimit caps the number of results a single search can return.
	MaxSearchLimit = 50

	// candidatePool is how many pages each FTS query may add for ranking,
	// and how many are enough to stop querying. matchPool is how many of a
	// query's matches are read to find them: a page can be stored as many
	// URLs, one per translation and version.
	candidatePool = 300
	matchPool     = 10 * candidatePool

	// maxLeaveOneOut is the most terms a query can have for the searches
	// that each ignore one of them.
	maxLeaveOneOut = 6

	// bm25 column weights for urls_fts (domain, segments, keywords). bm25
	// only picks the candidates of a search for any of the terms.
	weightDomain   = 0.5
	weightSegments = 1.0
	weightKeywords = 2.0

	// Score mix: how much of the query a URL covers, how much of the URL's
	// last segment the query accounts for, whether the terms sit together
	// in the URL as they do in the query, and how shallow the path is.
	mixCoverage  = 0.60
	mixFocus     = 0.20
	mixProximity = 0.15
	mixDepth     = 0.05

	// localeBias is the part of the score given to URLs in the user's
	// language, and regionBias the part given on top when the region matches
	// too. The mix above shares what is left, so scores stay within 1.
	localeBias = 0.10
	regionBias = 0.01

	// Credit for how a term matches a word of the URL, from the same word
	// down: its plural or singular, the same words joined or split
	// differently ("config map" and "configmap"), another inflection
	// ("configuring" for "configuration"), the start of a longer word, an
	// abbreviation ("params" for "parameters"), and a similar spelling.
	pluralMatchCredit   = 0.95
	compoundMatchCredit = 0.95
	stemMatchCredit     = 0.85
	prefixMatchCredit   = 0.7
	abbrevMatchCredit   = 0.6
	fuzzyMatchCredit    = 0.6
	// minPrefixLen is the shortest stem that is allowed to prefix-match.
	minPrefixLen = 3

	// Credit is scaled by where the term matched: the last path segment
	// names the page, earlier ones its section, and the domain is shared by
	// the whole site.
	fieldSlug   = 1.0
	fieldPath   = 0.9
	fieldDomain = 0.5

	// minTermWeight is the least a term can weigh relative to the rarest
	// term of the query, however common it is in the index.
	minTermWeight = 0.15

	// maxJoinTerms is the longest run of query terms tried as one word, and
	// maxSplitWords the longest run of URL words tried as one term.
	maxJoinTerms  = 3
	maxSplitWords = 4
	// minSplitLen is the shortest term looked up as two indexed words.
	minSplitLen = 6
	// minFuzzyLen is the shortest term corrected to a similar indexed word,
	// and maxSimilar how many such words are tried.
	minFuzzyLen = 5
	maxSimilar  = 3
)

// ErrEmptyQuery is returned when a search has neither terms nor a site.
var ErrEmptyQuery = errors.New("query has no search terms")

// SearchResult is one ranked URL.
type SearchResult struct {
	URL     string   `json:"url" jsonschema:"the page URL"`
	Score   float64  `json:"score" jsonschema:"relevance between 0 and 1, higher is better"`
	Matched []string `json:"matched" jsonschema:"query terms found in the URL"`
}

// candidate is a URL that matches the query, yet to be scored.
type candidate struct {
	scheme, domain, path string
	variant
	// bonus is the URL's localeBonus for the user.
	bonus float64
	// versionAsked is true when the query names the URL's version.
	versionAsked bool
}

func newCandidate(scheme, domain, path string, terms []queryTerm, user Locale) candidate {
	if user.Lang == "" {
		user = defaultLocale
	}
	c := candidate{
		scheme: scheme, domain: domain, path: path,
		variant: splitVariant(path),
		bonus:   localeBonus(path, user),
	}
	if words := tokenize(c.versionSegment, false); len(words) > 0 {
		c.versionAsked = true
		for _, word := range words {
			if !slices.ContainsFunc(terms, func(t queryTerm) bool { return t.text == word }) {
				c.versionAsked = false
			}
		}
	}
	return c
}

// suitsBetter reports whether c serves the user better than o, another
// variant of the same page: their language first, then a version the query
// asks for, then the newest version.
func (c candidate) suitsBetter(o candidate) bool {
	switch {
	case c.bonus != o.bonus:
		return c.bonus > o.bonus
	case c.versionAsked != o.versionAsked:
		return c.versionAsked
	}
	return c.version.newerThan(o.version)
}

// pageSet collects candidates, keeping the best variant of each page.
type pageSet struct {
	pages []candidate
	index map[string]int // domain and page to position in pages
}

// add puts c in the set and reports whether its page was new. A page that
// is new is dropped when room is false.
func (p *pageSet) add(c candidate, room bool) bool {
	key := c.domain + c.page
	if i, ok := p.index[key]; ok {
		if c.suitsBetter(p.pages[i]) {
			p.pages[i] = c
		}
		return false
	}
	if !room {
		return false
	}
	if p.index == nil {
		p.index = make(map[string]int)
	}
	p.index[key] = len(p.pages)
	p.pages = append(p.pages, c)
	return true
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

	terms, err := s.prepareTerms(ctx, q.Terms, site)
	if err != nil {
		return nil, err
	}
	candidates, err := s.candidates(ctx, terms, site)
	if err != nil {
		return nil, err
	}

	results := rank(terms, candidates)
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// queryTerm is a search term prepared for matching.
type queryTerm struct {
	text string
	stem string
	// weight is the term's share of the query; rarer terms weigh more.
	weight float64
	// docs estimates how many indexed URLs the term can match.
	docs int
	// splits are ways to write the term as two indexed words, such as
	// "cron job" for "cronjob".
	splits [][2]string
	// abbrevs are indexed words that abbreviate the term.
	abbrevs []string
	// similar holds indexed words a typo away, for a term that matches
	// nothing as written.
	similar []string
}

// newQueryTerms prepares terms with equal weights and no knowledge of the index.
func newQueryTerms(texts []string) []queryTerm {
	terms := make([]queryTerm, len(texts))
	for i, text := range texts {
		terms[i] = queryTerm{text: text, stem: stem(text), weight: 1 / float64(len(texts)), docs: 1}
	}
	return terms
}

// forms returns the spellings of the term that count as the same word.
func (t *queryTerm) forms() []string {
	forms := []string{t.text}
	add := func(f string) {
		if !slices.Contains(forms, f) {
			forms = append(forms, f)
		}
	}
	base := singular(t.text)
	add(base)
	for _, p := range plurals(base) {
		add(p)
	}
	return forms
}

// canPrefix reports whether the term's stem is long enough to prefix-match.
func (t *queryTerm) canPrefix() bool {
	return utf8.RuneCountInString(t.stem) >= minPrefixLen
}

// credit scores how well word stands for the term, from 0 to 1.
func (t *queryTerm) credit(word string) float64 {
	switch {
	case word == t.text:
		return 1
	case sameWord(word, t.text):
		return pluralMatchCredit
	case stem(word) == t.stem:
		return stemMatchCredit
	case t.canPrefix() && strings.HasPrefix(word, t.stem):
		return prefixMatchCredit
	case abbreviates(word, t.text):
		return abbrevMatchCredit
	case slices.Contains(t.similar, word), slices.Contains(t.similar, singular(word)):
		return fuzzyMatchCredit
	}
	return 0
}

// prepareTerms looks the query terms up in the index: how common each one
// is, which decides its weight, and how else it might be written there.
func (s *SQLiteStore) prepareTerms(ctx context.Context, texts []string, site string) ([]queryTerm, error) {
	terms := newQueryTerms(texts)

	// The highest id is as good a URL count as any here and, unlike
	// count(*), does not read the whole table.
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM urls`).Scan(&total); err != nil {
		return nil, fmt.Errorf("count urls: %w", err)
	}
	if total == 0 {
		return terms, nil
	}

	for i := range terms {
		t := &terms[i]
		var err error
		if t.docs, err = s.docFreq(ctx, t); err != nil {
			return nil, err
		}
		// A term the index does not have as a word may be a typo of one.
		if t.docs == 0 && utf8.RuneCountInString(t.text) >= minFuzzyLen {
			if t.similar, t.docs, err = s.similarWords(ctx, t.text); err != nil {
				return nil, err
			}
		}
		docs, err := s.respell(ctx, t)
		if err != nil {
			return nil, err
		}
		t.docs += docs
	}

	// Inverse document frequency, as in bm25. Every URL of the searched site
	// has the site's name in it, so a term from the name tells none apart.
	siteWords := tokenize(site, false)
	idf := make([]float64, len(terms))
	var maxIDF, sumFound float64
	found := 0
	for i, t := range terms {
		if t.docs == 0 || slices.Contains(siteWords, t.text) {
			continue
		}
		docs := float64(min(t.docs, total))
		idf[i] = math.Log(1 + (float64(total)-docs+0.5)/(docs+0.5))
		maxIDF = math.Max(maxIDF, idf[i])
		sumFound += idf[i]
		found++
	}
	if maxIDF == 0 {
		return terms, nil // nothing to tell the terms apart by
	}
	var sum float64
	for i, t := range terms {
		if t.docs == 0 {
			// A term the index lacks counts as an average one, so that it
			// lowers every score without drowning out the other terms.
			idf[i] = sumFound / float64(found)
		}
		idf[i] = math.Max(idf[i], minTermWeight*maxIDF)
		sum += idf[i]
	}
	for i := range terms {
		terms[i].weight = idf[i] / sum
	}
	return terms, nil
}

// docFreq estimates how many URLs contain a form of the term or a word that
// starts with its stem. A URL with several such words is counted for each.
func (s *SQLiteStore) docFreq(ctx context.Context, t *queryTerm) (int, error) {
	total := 0
	if t.canPrefix() {
		var n int
		err := s.db.QueryRowContext(ctx, `
			SELECT coalesce(sum(doc), 0) FROM urls_vocab
			WHERE term >= ?1 AND term < ?1 || char(0x10ffff)`, t.stem).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count urls with %q: %w", t.text, err)
		}
		total += n
	}
	for _, form := range t.forms() {
		if t.canPrefix() && strings.HasPrefix(form, t.stem) {
			continue // counted above
		}
		var n int
		err := s.db.QueryRowContext(ctx, `
			SELECT coalesce(sum(doc), 0) FROM urls_vocab WHERE term = ?`, form).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count urls with %q: %w", t.text, err)
		}
		total += n
	}
	return total, nil
}

// respell finds the other ways the index writes the term: as two words
// ("cron job" for "cronjob") or abbreviated ("params" for "parameters"). It
// fills t.splits and t.abbrevs and returns an upper bound on how many URLs
// they match.
func (s *SQLiteStore) respell(ctx context.Context, t *queryTerm) (docs int, err error) {
	const (
		minPart = 3
		maxLen  = 40 // longer than any word; bounds the lookup
	)
	term := t.text
	if !isASCII(term) || len(term) > maxLen {
		return 0, nil
	}

	// One lookup for every word that could be part of a respelling.
	var lookups []any
	canSplit := len(term) >= minSplitLen
	if canSplit {
		for i := minPart; i <= len(term)-minPart; i++ {
			lookups = append(lookups, term[:i], term[i:])
		}
	}
	// Longer prefixes than the stem are found by prefix matching.
	for i := minAbbrevLen; i < len(term) && i < len(t.stem); i++ {
		lookups = append(lookups, term[:i], term[:i]+"s")
	}
	if len(lookups) == 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT term, doc FROM urls_vocab
		WHERE term IN (?`+strings.Repeat(`, ?`, len(lookups)-1)+`)`, lookups...)
	if err != nil {
		return 0, fmt.Errorf("respell %q: %w", term, err)
	}
	defer rows.Close()

	indexed := make(map[string]int)
	for rows.Next() {
		var word string
		var n int
		if err := rows.Scan(&word, &n); err != nil {
			return 0, fmt.Errorf("respell %q: %w", term, err)
		}
		indexed[word] = n
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("respell %q: %w", term, err)
	}

	if canSplit {
		for i := minPart; i <= len(term)-minPart; i++ {
			left, right := indexed[term[:i]], indexed[term[i:]]
			if left > 0 && right > 0 {
				t.splits = append(t.splits, [2]string{term[:i], term[i:]})
				docs += min(left, right)
			}
		}
	}
	for i := minAbbrevLen; i < len(term) && i < len(t.stem); i++ {
		for _, word := range []string{term[:i], term[:i] + "s"} {
			if n := indexed[word]; n > 0 && abbreviates(word, term) {
				t.abbrevs = append(t.abbrevs, word)
				docs += n
			}
		}
	}
	return docs, nil
}

// similarWords returns the indexed words closest to a term that is not in
// the index, likely a typo of them, and how many URLs contain them.
func (s *SQLiteStore) similarWords(ctx context.Context, term string) (words []string, docs int, err error) {
	runes := []rune(term)
	maxDistance := 1
	if len(runes) >= 8 {
		maxDistance = 2
	}

	// Typos rarely hit the first letter, which keeps the scan small.
	first := string(runes[0])
	rows, err := s.db.QueryContext(ctx, `
		SELECT term, doc FROM urls_vocab
		WHERE term >= ?1 AND term < ?1 || char(0x10ffff)`, first)
	if err != nil {
		return nil, 0, fmt.Errorf("find words like %q: %w", term, err)
	}
	defer rows.Close()

	type match struct {
		word           string
		distance, docs int
	}
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.word, &m.docs); err != nil {
			return nil, 0, fmt.Errorf("find words like %q: %w", term, err)
		}
		if m.distance = editDistance(term, m.word, maxDistance); m.distance <= maxDistance {
			matches = append(matches, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("find words like %q: %w", term, err)
	}

	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.distance != b.distance {
			return a.distance < b.distance
		}
		if a.docs != b.docs {
			return a.docs > b.docs
		}
		return a.word < b.word
	})
	if len(matches) > maxSimilar {
		matches = matches[:maxSimilar]
	}
	for _, m := range matches {
		words = append(words, m.word)
		docs += m.docs
	}
	return words, docs, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// join is a run of adjacent query terms written as one word, which is how a
// URL may spell them: "config map" as "configmap".
type join struct {
	from, to int // first and last term of the run
	text     string
}

func joins(terms []queryTerm) []join {
	var out []join
	for from := range terms {
		text := terms[from].text
		for to := from + 1; to < len(terms) && to-from < maxJoinTerms; to++ {
			text += terms[to].text
			out = append(out, join{from: from, to: to, text: text})
		}
	}
	return out
}

// matchPass is a set of FTS5 MATCH expressions that are equally strict.
type matchPass struct {
	exprs []string
	// partial is true when a match may hold only some of the terms.
	partial bool
}

// matchPasses builds the FTS5 MATCH expressions for terms in passes, the
// strictest first: every term as written, every term in any inflection,
// every term but one, and any term. A pass only runs when the ones before
// it found too little.
func matchPasses(terms []queryTerm) []matchPass {
	quote := func(s string) string { return `"` + s + `"` }

	runs := joins(terms)
	// The terms the index has, as written and in any inflection. A term it
	// lacks would make a search for all of them match nothing.
	var exact, inflected []string
	var any []string
	for i := range terms {
		t := &terms[i]
		var alts []string
		for _, form := range t.forms() {
			alts = append(alts, quote(form))
		}
		for _, run := range runs {
			if run.from <= i && i <= run.to {
				alts = append(alts, quote(run.text)+`*`)
			}
		}
		for _, split := range t.splits {
			alts = append(alts, quote(split[0])+` + `+quote(split[1])+`*`)
		}
		strict := `(` + strings.Join(alts, " OR ") + `)`
		for _, word := range t.abbrevs {
			alts = append(alts, quote(word))
		}
		for _, word := range t.similar {
			alts = append(alts, quote(word))
		}
		if t.canPrefix() {
			alts = append(alts, quote(t.stem)+`*`)
		}
		loose := `(` + strings.Join(alts, " OR ") + `)`

		any = append(any, loose)
		if t.docs > 0 {
			exact = append(exact, strict)
			inflected = append(inflected, loose)
		}
	}

	var leaveOneOut []string
	if n := len(inflected); n >= 3 && n <= maxLeaveOneOut {
		for skip := range inflected {
			rest := slices.Delete(slices.Clone(inflected), skip, skip+1)
			leaveOneOut = append(leaveOneOut, strings.Join(rest, " AND "))
		}
	}

	var passes []matchPass
	var seen []string
	for _, pass := range []matchPass{
		{exprs: []string{strings.Join(exact, " AND ")}},
		{exprs: []string{strings.Join(inflected, " AND ")}},
		{exprs: leaveOneOut},
		{exprs: []string{strings.Join(any, " OR ")}, partial: true},
	} {
		var exprs []string
		for _, expr := range pass.exprs {
			if expr != "" && !slices.Contains(seen, expr) {
				seen = append(seen, expr)
				exprs = append(exprs, expr)
			}
		}
		if len(exprs) > 0 {
			passes = append(passes, matchPass{exprs: exprs, partial: pass.partial})
		}
	}
	return passes
}

// candidates pulls the pages worth ranking, one variant of each: the best
// matches of each pass of matchPasses in turn, until there are enough.
func (s *SQLiteStore) candidates(ctx context.Context, terms []queryTerm, site string) ([]candidate, error) {
	var set pageSet
	for _, pass := range matchPasses(terms) {
		for _, expr := range pass.exprs {
			if err := s.match(ctx, expr, pass.partial, site, terms, &set); err != nil {
				return nil, err
			}
		}
		if len(set.pages) >= candidatePool {
			break
		}
	}
	return set.pages, nil
}

// match adds the best matches of one FTS5 expression to set. Matches that
// may hold only some of the terms are taken in bm25 order, which favors the
// ones with more and rarer terms. Matches that hold them all differ mostly
// in how much else their URL says, so the shortest are taken; that is also
// far cheaper than bm25 when a query of common words matches most of a site.
func (s *SQLiteStore) match(ctx context.Context, expr string, partial bool, site string, terms []queryTerm, set *pageSet) error {
	sqlText := `
		SELECT u.scheme, u.domain, u.path, length(u.path) AS rank
		FROM urls_fts
		JOIN urls u ON u.id = urls_fts.rowid
		WHERE urls_fts MATCH ?`
	args := []any{expr}
	if partial {
		sqlText = `
			SELECT u.scheme, u.domain, u.path, bm25(urls_fts, ?, ?, ?) AS rank
			FROM urls_fts
			JOIN urls u ON u.id = urls_fts.rowid
			WHERE urls_fts MATCH ?`
		args = []any{weightDomain, weightSegments, weightKeywords, expr}
	}
	if site != "" {
		sqlText += ` AND (u.domain = ? OR u.domain LIKE '%.' || ?)`
		args = append(args, site, site)
	}
	sqlText += ` ORDER BY rank LIMIT ?`
	args = append(args, matchPool)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return fmt.Errorf("search urls: %w", err)
	}
	defer rows.Close()

	added := 0
	for rows.Next() {
		var scheme, domain, path string
		var rank float64
		if err := rows.Scan(&scheme, &domain, &path, &rank); err != nil {
			return fmt.Errorf("scan search result: %w", err)
		}
		// Past the pool, rows still matter as variants of the pages in it.
		if set.add(newCandidate(scheme, domain, path, terms, s.Locale), added < candidatePool) {
			added++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("search urls: %w", err)
	}
	return nil
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

// pathWord is one word of a URL path.
type pathWord struct {
	text    string
	segment int // index of the path segment the word is in
}

// pathWords returns the words of path in order, without the stopwords and
// opaque identifiers that the query does not ask for.
func pathWords(path string, terms []queryTerm) []pathWord {
	asked := func(word string) bool {
		for i := range terms {
			if terms[i].text == word {
				return true
			}
		}
		return false
	}
	var words []pathWord
	for i, segment := range strings.Split(path, "/") {
		for _, word := range tokenize(stripPageExtension(segment), true) {
			if (isStopword(word) || isNoise(word)) && !asked(word) {
				continue
			}
			words = append(words, pathWord{text: word, segment: i})
		}
	}
	return words
}

// span is a run of path words that matches a query term.
type span struct {
	from, to int // first and last word
	credit   float64
}

// relevance is how well one URL answers the query.
type relevance struct {
	// coverage is the weighted share of the query found in the URL.
	coverage float64
	// focus is the share of the last path segment that the query accounts for.
	focus float64
	// proximity is 1 when neighboring query terms are neighbors in the URL
	// too, and less for each pair that is not.
	proximity float64
	matched   []string
}

// score combines the parts of a relevance into one number between 0 and 1.
func (r relevance) score(depth int) float64 {
	return mixCoverage*r.coverage +
		mixFocus*r.coverage*r.focus +
		mixProximity*r.proximity +
		mixDepth/float64(1+depth)
}

// assess measures how well the URL made of domain and path answers terms.
func assess(terms []queryTerm, runs []join, domain, path string) relevance {
	words := pathWords(path, terms)
	slug := -1 // segment of the last word: the page's own name
	if len(words) > 0 {
		slug = words[len(words)-1].segment
	}

	// Every place in the path where each term matches.
	spans := make([][]span, len(terms))
	for k := range terms {
		t := &terms[k]
		for i, w := range words {
			if c := t.credit(w.text); c > 0 {
				spans[k] = append(spans[k], span{i, i, c})
			}
			// The URL may split the term into several words.
			joined := w.text
			for j := i + 1; j < len(words) && j-i < maxSplitWords &&
				words[j].segment == w.segment && len(joined) < len(t.text); j++ {
				joined += words[j].text
				if sameWord(joined, t.text) {
					spans[k] = append(spans[k], span{i, j, compoundMatchCredit})
				}
			}
		}
	}
	// The URL may join several terms into one word.
	for _, run := range runs {
		for i, w := range words {
			var c float64
			switch {
			case sameWord(w.text, run.text):
				c = compoundMatchCredit
			case strings.HasPrefix(w.text, run.text):
				c = prefixMatchCredit
			default:
				continue
			}
			for k := run.from; k <= run.to; k++ {
				spans[k] = append(spans[k], span{i, i, c})
			}
		}
	}

	var r relevance
	r.matched = []string{}
	domainWords := tokenize(domain, false)
	inSlug := make(map[int]struct{})
	for k := range terms {
		t := &terms[k]
		best := 0.0
		for _, word := range domainWords {
			best = math.Max(best, fieldDomain*t.credit(word))
		}
		for _, sp := range spans[k] {
			field := fieldPath
			if words[sp.to].segment == slug {
				field = fieldSlug
			}
			best = math.Max(best, field*sp.credit)
			for i := sp.from; i <= sp.to; i++ {
				if words[i].segment == slug {
					inSlug[i] = struct{}{}
				}
			}
		}
		if best > 0 {
			r.coverage += t.weight * best
			r.matched = append(r.matched, t.text)
		}
	}

	slugWords := 0
	for _, w := range words {
		if w.segment == slug {
			slugWords++
		}
	}
	if slugWords > 0 {
		r.focus = float64(len(inSlug)) / float64(slugWords)
	}

	// Each pair of neighboring terms that the URL does not keep together
	// costs proximity in proportion to how much the pair matters; being
	// next to a term that barely counts is no achievement.
	r.proximity = 1
	for k := 0; k+1 < len(terms); k++ {
		near := 0.0
		for _, a := range spans[k] {
			for _, b := range spans[k+1] {
				closeness := 0.0
				switch {
				case a.to+1 == b.from, a == b:
					closeness = 1 // side by side and in order, or one word for both
				case words[a.to].segment == words[b.from].segment:
					closeness = 0.5
				}
				near = math.Max(near, closeness*math.Min(a.credit, b.credit))
			}
		}
		wa, wb := terms[k].weight, terms[k+1].weight
		pair := 2 * math.Min(wa, wb) / (wa + wb)
		r.proximity -= pair * (1 - near) / float64(len(terms)-1)
	}
	return r
}

// rank scores candidates against the query terms and orders them best first.
func rank(terms []queryTerm, candidates []candidate) []SearchResult {
	type scored struct {
		SearchResult
		score float64 // before rounding
	}
	results := make([]scored, 0, len(candidates))

	runs := joins(terms)
	for _, c := range candidates {
		// The locale and version do not make a page any deeper.
		rel := assess(terms, runs, c.domain, c.path)
		score := (1-localeBias-regionBias)*rel.score(pathDepth(c.page)) + c.bonus
		results = append(results, scored{
			SearchResult: SearchResult{
				URL:     buildURL(c.scheme, c.domain, c.path),
				Score:   round3(score),
				Matched: rel.matched,
			},
			score: score,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if len(a.URL) != len(b.URL) {
			return len(a.URL) < len(b.URL)
		}
		return a.URL < b.URL
	})

	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = r.SearchResult
	}
	return out
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
