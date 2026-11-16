package index

import (
	"strings"
	"unicode"
)

// stopwords carry no search signal in a URL path or a query.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"by": {}, "for": {}, "from": {}, "how": {}, "in": {}, "index": {},
	"is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "that": {}, "the": {},
	"this": {}, "to": {}, "was": {}, "what": {}, "with": {},
}

// pageExtensions describe how a page is served; content types like pdf are kept.
var pageExtensions = map[string]struct{}{
	"html": {}, "htm": {}, "xhtml": {}, "shtml": {}, "php": {}, "asp": {},
	"aspx": {}, "jsp": {}, "cfm": {},
}

// maxQueryTerms bounds the size of the FTS expression built from a query.
const maxQueryTerms = 16

// tokenize splits s into lower-cased alphanumeric tokens. With splitCamel,
// camelCase words are split too, as are acronyms from the word that follows:
// "getHTMLInputElement" becomes get, html, input, element.
func tokenize(s string, splitCamel bool) []string {
	var (
		out []string
		cur []rune
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if n := len(cur); splitCamel && n > 0 {
			switch {
			case unicode.IsUpper(r) && unicode.IsLower(cur[n-1]):
				flush()
			case unicode.IsLower(r) && n >= 3 && unicode.IsUpper(cur[n-1]) &&
				unicode.IsUpper(cur[n-2]) && unicode.IsUpper(cur[n-3]):
				// The last capital starts a new word. Two capitals are
				// left alone, as in "OAuth".
				last := cur[n-1]
				cur = cur[:n-1]
				flush()
				cur = append(cur, last)
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func isStopword(token string) bool {
	_, ok := stopwords[token]
	return ok
}

// isNoise reports whether a token is an opaque identifier.
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

// ExtractKeywords returns the distinct keywords found in a URL path.
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

// ParseQuery splits a query into terms and an optional site restriction.
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

// NormalizeSite returns the lower-cased host without scheme, port, path, or www.
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

// singular strips common English plural endings.
func singular(term string) string {
	n := len(term)
	if n < 4 {
		return term
	}
	switch {
	case n >= 5 && strings.HasSuffix(term, "ies"):
		return term[:n-3] + "y"
	case strings.HasSuffix(term, "sses"), strings.HasSuffix(term, "ches"),
		strings.HasSuffix(term, "shes"), strings.HasSuffix(term, "xes"):
		return term[:n-2]
	case strings.HasSuffix(term, "ss"), strings.HasSuffix(term, "us"),
		strings.HasSuffix(term, "is"):
		return term
	case strings.HasSuffix(term, "s"):
		return term[:n-1]
	}
	return term
}

// plurals returns the plural spellings of a singular word.
func plurals(word string) []string {
	n := len(word)
	if n < 3 {
		return nil
	}
	switch {
	case word[n-1] == 'y' && !isVowel(word[n-2]):
		return []string{word[:n-1] + "ies"}
	case strings.HasSuffix(word, "s"), strings.HasSuffix(word, "x"),
		strings.HasSuffix(word, "ch"), strings.HasSuffix(word, "sh"):
		return []string{word + "es"}
	}
	return []string{word + "s"}
}

// sameWord reports whether a and b are the same word, ignoring plurals.
func sameWord(a, b string) bool {
	return a == b || singular(a) == singular(b) ||
		strings.TrimSuffix(a, "s") == strings.TrimSuffix(b, "s")
}

// minAbbrevLen is the shortest word taken as an abbreviation of a longer one.
const minAbbrevLen = 4

// abbreviates reports whether word is a shortened form of term, the way
// URLs write "params" for "parameters" or "auth" for "authentication".
func abbreviates(word, term string) bool {
	word = singular(word)
	return len(word) >= minAbbrevLen && len(word) < len(term) && strings.HasPrefix(term, word)
}

// stem reduces an English word to a root shared by its common inflections:
// "configuring", "configured" and "configuration" all become "configur". The
// root is meant for comparing and prefix matching and is often not a word.
func stem(term string) string {
	s := singular(term)
	// cut removes suffix when at least min bytes would remain.
	cut := func(suffix string, min int) bool {
		if strings.HasSuffix(s, suffix) && len(s)-len(suffix) >= min {
			s = s[:len(s)-len(suffix)]
			return true
		}
		return false
	}
	switch {
	case cut("ation", 4), cut("ment", 5), cut("ate", 4), cut("ion", 5):
	case cut("ing", 4), cut("ed", 4):
		// "migrating" -> "migrat" -> "migr", "logging" -> "logg" -> "log".
		if n := len(s); !cut("at", 4) && s[n-1] == s[n-2] &&
			!isVowel(s[n-1]) && !strings.ContainsRune("lsz", rune(s[n-1])) {
			s = s[:n-1]
		}
	}
	if n := len(s); n >= 5 && (s[n-1] == 'e' || (s[n-1] == 'y' && !isVowel(s[n-2]))) {
		s = s[:n-1]
	}
	return s
}

func isVowel(c byte) bool {
	return strings.IndexByte("aeiouy", c) >= 0
}

// editDistance returns the number of single-character insertions, deletions,
// substitutions and adjacent transpositions that turn a into b, or max+1 as
// soon as it is known to exceed max.
func editDistance(a, b string, max int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > max || -d > max {
		return max + 1
	}
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
			best = min(best, cur[j])
		}
		if best > max {
			return max + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

// Locale is a language with an optional region, both lower-cased.
type Locale struct {
	Lang   string // ISO 639-1 code, e.g. "en"
	Region string // e.g. "us"; "" when unknown
}

// defaultLocale is assumed when the user's locale is unknown.
var defaultLocale = Locale{Lang: "en"}

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

// ParseLocale reads a locale such as "en_US.UTF-8", "pt-BR" or "de". It
// reports false for values that name no language, like "C" or "POSIX".
func ParseLocale(s string) (Locale, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, ".@"); i >= 0 {
		s = s[:i] // drop the encoding and modifier
	}
	lang, region, _ := strings.Cut(strings.ReplaceAll(s, "_", "-"), "-")
	if _, ok := languageCodes[lang]; !ok {
		return Locale{}, false
	}
	return Locale{Lang: lang, Region: region}, true
}

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

// localeBonus is the score added to a URL for how well the locale in its path
// suits the user: the full bonus for the user's language (a little more when
// the region matches too), nothing for another language. A path without a
// locale is the site's default language, which is assumed to be English.
func localeBonus(path string, user Locale) float64 {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !isLocale(first) {
		if user.Lang == defaultLocale.Lang {
			return localeBias
		}
		return localeBias / 2
	}
	page, _ := ParseLocale(first)
	switch {
	case page.Lang != user.Lang:
		return 0
	case page.Region != "" && page.Region == user.Region:
		return localeBias + regionBias
	}
	return localeBias
}

// version is a documentation version named by a path segment such as "5.2",
// "v2", "stable" or "dev".
type version struct {
	// channel orders kinds of version: releaseChannel for numbered releases,
	// above it aliases of the newest release, below it unreleased builds.
	channel int
	// numbers are the components of a numbered release.
	numbers []int
}

const (
	unreleasedChannel = iota
	releaseChannel
	currentChannel
)

// versionChannels maps the names sites use for unnumbered versions.
var versionChannels = map[string]int{
	"latest": currentChannel, "stable": currentChannel, "current": currentChannel,
	"dev": unreleasedChannel, "devel": unreleasedChannel, "main": unreleasedChannel,
	"master": unreleasedChannel, "next": unreleasedChannel, "nightly": unreleasedChannel,
	"canary": unreleasedChannel, "beta": unreleasedChannel, "alpha": unreleasedChannel,
	"edge": unreleasedChannel, "unstable": unreleasedChannel, "trunk": unreleasedChannel,
	"snapshot": unreleasedChannel,
}

// unversioned is the version of a path that names none, which is taken to be
// the site's current documentation.
var unversioned = version{channel: currentChannel}

// parseVersion reads a path segment as a version. Numbers must be dotted or
// carry a "v" ("5.2", "v2", "2.x"); a bare number is as likely a page or an ID.
func parseVersion(segment string) (version, bool) {
	s := strings.ToLower(segment)
	if channel, ok := versionChannels[s]; ok {
		return version{channel: channel}, true
	}
	rest := strings.TrimPrefix(s, "v")
	if rest == s && !strings.Contains(s, ".") {
		return version{}, false
	}
	v := version{channel: releaseChannel}
	for i, part := range strings.Split(rest, ".") {
		if part == "x" && i > 0 {
			break // "2.x"
		}
		if part == "" || len(part) > 4 {
			return version{}, false
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return version{}, false
			}
			n = n*10 + int(r-'0')
		}
		v.numbers = append(v.numbers, n)
	}
	return v, len(v.numbers) > 0
}

// newerThan reports whether v is the version a reader more likely wants than
// o: the current release, then the highest numbered one, then unreleased
// builds. A shorter number is the newest of its series ("3" over "3.12").
func (v version) newerThan(o version) bool {
	if v.channel != o.channel {
		return v.channel > o.channel
	}
	for i := 0; i < len(v.numbers) && i < len(o.numbers); i++ {
		if v.numbers[i] != o.numbers[i] {
			return v.numbers[i] > o.numbers[i]
		}
	}
	return len(v.numbers) < len(o.numbers)
}

// versionDepth is how many leading path segments, after any locale, are
// searched for a version.
const versionDepth = 3

// variant is what sets a URL apart from other renderings of the same page.
type variant struct {
	// page is the path without its locale and version segments; it is the
	// same for every variant of a page.
	page    string
	version version
	// versionSegment is the path segment that names the version, or "".
	versionSegment string
}

// splitVariant separates a path from the leading segments that select a
// variant of the page: its locale ("/fr/docs/x") and version ("/docs/5.2/x").
func splitVariant(path string) variant {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if isLocale(segments[0]) {
		segments = segments[1:]
	}
	v := variant{version: unversioned}
	for i := 0; i < len(segments) && i < versionDepth; i++ {
		if version, ok := parseVersion(segments[i]); ok {
			v.version, v.versionSegment = version, segments[i]
			segments = append(segments[:i:i], segments[i+1:]...)
			break
		}
	}
	v.page = "/" + strings.Join(segments, "/")
	return v
}
