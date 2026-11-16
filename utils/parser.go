package utils

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// URL is the normalized form stored in the SQLite urls table.
//
// Domain and Path are the identity of a page (enforced unique by the
// schema); Segments and Keywords are pre-computed search terms so the
// FTS5 index can answer queries without re-parsing paths.
type URL struct {
	Scheme   string
	Domain   string
	Path     string
	Segments string
	Keywords string
}

// ParsePageURL parses an absolute http(s) URL into its normalized store
// representation. Query strings and fragments are dropped, hosts are
// lower-cased, and trailing slashes are removed from non-root paths.
func ParsePageURL(raw string) (URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return URL{}, fmt.Errorf("parse url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return URL{}, fmt.Errorf("unsupported scheme %q in %q", u.Scheme, raw)
	}

	domain := strings.ToLower(u.Hostname())
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return URL{}, fmt.Errorf("url %q has no host", raw)
	}

	path := u.Path
	if path == "" {
		path = "/"
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}

	segments := pathSegments(path)
	return URL{
		Scheme:   u.Scheme,
		Domain:   domain,
		Path:     path,
		Segments: segments,
		Keywords: ExtractKeywords(path),
	}, nil
}

func pathSegments(path string) string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, " ")
}

// ParseRobots extracts the value of every Sitemap directive from a
// robots.txt document. Other directives (User-agent, Allow, Disallow,
// Crawl-delay) are intentionally ignored because they do not identify
// concrete URLs.
func ParseRobots(body []byte) []string {
	var sitemaps []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key == "sitemap" && value != "" {
			sitemaps = append(sitemaps, value)
		}
	}
	return sitemaps
}

type xmlSitemapIndex struct {
	XMLName  xml.Name `xml:"sitemapindex"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

type xmlURLSet struct {
	XMLName xml.Name `xml:"urlset"`
	URLs    []struct {
		Loc string `xml:"loc"`
	} `xml:"url"`
}

// ParseSitemap parses a sitemap document, returning the page URLs it lists
// and any nested sitemap URLs it points to. Both urlset and sitemapindex
// documents are supported, and gzip-compressed bodies are decompressed
// transparently.
func ParseSitemap(body []byte) (pageURLs []string, sitemapURLs []string, err error) {
	body, err = maybeGunzip(body)
	if err != nil {
		return nil, nil, err
	}

	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(body, &probe); err != nil {
		return nil, nil, fmt.Errorf("parse sitemap xml: %w", err)
	}

	switch probe.XMLName.Local {
	case "urlset":
		var doc xmlURLSet
		if err := xml.Unmarshal(body, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse urlset: %w", err)
		}
		for _, u := range doc.URLs {
			if loc := strings.TrimSpace(u.Loc); loc != "" {
				pageURLs = append(pageURLs, loc)
			}
		}
	case "sitemapindex":
		var doc xmlSitemapIndex
		if err := xml.Unmarshal(body, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse sitemapindex: %w", err)
		}
		for _, s := range doc.Sitemaps {
			if loc := strings.TrimSpace(s.Loc); loc != "" {
				sitemapURLs = append(sitemapURLs, loc)
			}
		}
	default:
		return nil, nil, fmt.Errorf("unexpected sitemap root element %q", probe.XMLName.Local)
	}

	return pageURLs, sitemapURLs, nil
}

const maxDecompressedSitemap = 64 << 20 // 64 MiB

// maybeGunzip decompresses body when it begins with the gzip magic bytes,
// returning body unchanged otherwise. The decompressed size is bounded to
// guard against decompression bombs.
func maybeGunzip(body []byte) ([]byte, error) {
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("open gzip sitemap: %w", err)
	}
	defer zr.Close()

	out, err := io.ReadAll(io.LimitReader(zr, maxDecompressedSitemap+1))
	if err != nil {
		return nil, fmt.Errorf("read gzip sitemap: %w", err)
	}
	if int64(len(out)) > maxDecompressedSitemap {
		return nil, fmt.Errorf("decompressed sitemap exceeds %d bytes", maxDecompressedSitemap)
	}
	return out, nil
}
