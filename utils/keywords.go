package utils

import (
	"strings"
	"unicode"
)

// stopwords carry no search signal in a URL path or a query. "index" is here
// because /docs/index.html says nothing beyond /docs.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"by": {}, "for": {}, "from": {}, "how": {}, "in": {}, "index": {},
	"is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {},
	"this": {}, "to": {}, "was": {}, "what": {}, "with": {},
}

// pageExtensions are file extensions that describe how a page is served
// rather than what it is about. Content types such as pdf are kept.
var pageExtensions = map[string]struct{}{
	"html": {}, "htm": {}, "xhtml": {}, "shtml": {}, "php": {}, "asp": {},
	"aspx": {}, "jsp": {}, "cfm": {},
}

// maxQueryTerms bounds the size of the FTS expression built from a query.
const maxQueryTerms = 16

// tokenize lower-cases s and splits it on anything that is not a letter or
// digit. With splitCamel, a lower-to-upper transition also starts a new
// token, so "getUserName" yields get, user, name.
func tokenize(s string, splitCamel bool) []string {
	var (
		out  []string
		cur  []rune
		prev rune
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if splitCamel && unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur = append(cur, r)
		} else {
			flush()
		}
		prev = r
	}
	flush()
	return out
}

func isStopword(token string) bool {
	_, ok := stopwords[token]
	return ok
}

// isNoise reports whether a path token is an opaque identifier rather than a
// word: a single letter, a long number (short ones are kept, since years and
// versions are meaningful), or a hex hash.
func isNoise(token string) bool {
	runes := []rune(token)
	if len(runes) == 1 {
		return !unicode.IsDigit(runes[0])
	}

	digits, hex := 0, true
	for _, r := range runes {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'a' && r <= 'f':
		default:
			hex = false
		}
	}
	if digits == len(runes) {
		return digits > 4
	}
	return hex && digits > 0 && len(runes) >= 8
}

// stripPageExtension removes a trailing page extension from a path segment.
func stripPageExtension(segment string) string {
	i := strings.LastIndexByte(segment, '.')
	if i <= 0 {
		return segment
	}
	if _, ok := pageExtensions[strings.ToLower(segment[i+1:])]; ok {
		return segment[:i]
	}
	return segment
}

// ExtractKeywords returns the distinct search keywords found in a URL path,
// in order of first appearance, joined by spaces. Page extensions, stopwords
// and opaque identifiers are dropped, and camelCase words are split.
func ExtractKeywords(path string) string {
	seen := make(map[string]struct{})
	var out []string
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		for _, token := range tokenize(stripPageExtension(segment), true) {
			if isStopword(token) || isNoise(token) {
				continue
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			out = append(out, token)
		}
	}
	return strings.Join(out, " ")
}

// Query is a parsed search request.
type Query struct {
	// Terms are the distinct lower-cased search terms.
	Terms []string
	// Site is the host from a "site:" operator, or "" if none was given.
	Site string
}

// ParseQuery splits a free-text query into search terms and an optional
// "site:example.com" restriction. Stopwords are dropped unless the query
// consists of nothing else.
func ParseQuery(raw string) Query {
	var (
		q   Query
		all []string
	)
	for _, field := range strings.Fields(raw) {
		if len(field) > 5 && strings.EqualFold(field[:5], "site:") {
			q.Site = NormalizeSite(field[5:])
			continue
		}
		all = append(all, tokenize(field, false)...)
	}

	seen := make(map[string]struct{}, len(all))
	var kept, dropped []string
	for _, token := range all {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		if isStopword(token) {
			dropped = append(dropped, token)
		} else {
			kept = append(kept, token)
		}
	}
	if len(kept) == 0 {
		kept = dropped
	}
	if len(kept) > maxQueryTerms {
		kept = kept[:maxQueryTerms]
	}
	q.Terms = kept
	return q
}

// NormalizeSite reduces a host or URL to the key used to identify a site:
// lower-cased host without scheme, port, path or a leading "www.".
func NormalizeSite(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 && !strings.Contains(s, "]") {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	return strings.TrimPrefix(s, "www.")
}

// stem strips common English plural endings so that a query for "widgets"
// can prefix-match "widget". It is deliberately conservative.
func stem(term string) string {
	n := len(term)
	if n < 4 {
		return term
	}
	switch {
	case n >= 5 && strings.HasSuffix(term, "ies"):
		return term[:n-3]
	case strings.HasSuffix(term, "ches"), strings.HasSuffix(term, "shes"),
		strings.HasSuffix(term, "xes"), strings.HasSuffix(term, "zes"),
		strings.HasSuffix(term, "ses"):
		return term[:n-2]
	case strings.HasSuffix(term, "ss"), strings.HasSuffix(term, "us"),
		strings.HasSuffix(term, "is"):
		return term
	case strings.HasSuffix(term, "s"):
		return term[:n-1]
	}
	return term
}
