package index

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS urls (
	id       INTEGER PRIMARY KEY,
	scheme   TEXT NOT NULL DEFAULT 'https',
	domain   TEXT NOT NULL,
	path     TEXT NOT NULL,
	segments TEXT NOT NULL DEFAULT '',
	keywords TEXT NOT NULL DEFAULT '',
	UNIQUE (domain, path)
);

CREATE TABLE IF NOT EXISTS sites (
	domain     TEXT PRIMARY KEY,
	crawled_at INTEGER NOT NULL,
	url_count  INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE IF NOT EXISTS urls_fts USING fts5(
	domain,
	segments,
	keywords,
	content = 'urls',
	content_rowid = 'id',
	tokenize = 'unicode61 remove_diacritics 2'
);

-- Per-word document counts of urls_fts, for weighing and correcting query terms.
CREATE VIRTUAL TABLE IF NOT EXISTS urls_vocab USING fts5vocab(urls_fts, 'row');

CREATE TRIGGER IF NOT EXISTS urls_ai AFTER INSERT ON urls BEGIN
	INSERT INTO urls_fts (rowid, domain, segments, keywords)
	VALUES (new.id, new.domain, new.segments, new.keywords);
END;

CREATE TRIGGER IF NOT EXISTS urls_ad AFTER DELETE ON urls BEGIN
	INSERT INTO urls_fts (urls_fts, rowid, domain, segments, keywords)
	VALUES ('delete', old.id, old.domain, old.segments, old.keywords);
END;
`

type SQLiteStore struct {
	db *sql.DB

	// Locale is the user's locale; Search favors pages that match it. The
	// zero value means English.
	Locale Locale
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// InsertURLs inserts the given URLs in a single transaction, skipping duplicates.
func (s *SQLiteStore) InsertURLs(ctx context.Context, urls []URL) (int, error) {
	if len(urls) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin insert urls: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO urls (scheme, domain, path, segments, keywords)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (domain, path) DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("prepare insert urls: %w", err)
	}
	defer stmt.Close()

	var inserted int64
	for _, u := range urls {
		res, err := stmt.ExecContext(ctx, schemeOrDefault(u.Scheme), u.Domain, u.Path, u.Segments, u.Keywords)
		if err != nil {
			return 0, fmt.Errorf("insert url %s%s: %w", u.Domain, u.Path, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected for %s%s: %w", u.Domain, u.Path, err)
		}
		inserted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit insert urls: %w", err)
	}
	return int(inserted), nil
}

func schemeOrDefault(scheme string) string {
	if scheme == "" {
		return "https"
	}
	return scheme
}

// Site describes one crawled site.
type Site struct {
	Domain    string `json:"domain"`
	URLCount  int    `json:"url_count"`
	CrawledAt int64  `json:"crawled_at"`
}

// MarkSiteCrawled records that domain has been crawled.
func (s *SQLiteStore) MarkSiteCrawled(ctx context.Context, domain string, urlCount int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sites (domain, crawled_at, url_count)
		VALUES (?, unixepoch(), ?)
		ON CONFLICT (domain) DO UPDATE SET
			crawled_at = excluded.crawled_at,
			url_count  = excluded.url_count`,
		NormalizeSite(domain), urlCount)
	if err != nil {
		return fmt.Errorf("mark site %s crawled: %w", domain, err)
	}
	return nil
}

// SiteCrawled reports whether domain itself has already been crawled.
func (s *SQLiteStore) SiteCrawled(ctx context.Context, domain string) (bool, error) {
	site := NormalizeSite(domain)
	var crawled bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sites WHERE domain = ?)`, site).Scan(&crawled)
	if err != nil {
		return false, fmt.Errorf("check site %s crawled: %w", domain, err)
	}
	return crawled, nil
}

// Sites lists the crawled sites, most recently crawled first.
func (s *SQLiteStore) Sites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT domain, url_count, crawled_at FROM sites
		ORDER BY crawled_at DESC, domain`)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()

	sites := []Site{}
	for rows.Next() {
		var site Site
		if err := rows.Scan(&site.Domain, &site.URLCount, &site.CrawledAt); err != nil {
			return nil, fmt.Errorf("scan site: %w", err)
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

// CountURLs returns the number of stored URLs.
func (s *SQLiteStore) CountURLs(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM urls`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count urls: %w", err)
	}
	return n, nil
}

// Flush removes every stored URL and crawl record, leaving an empty store.
func (s *SQLiteStore) Flush(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin flush: %w", err)
	}
	defer tx.Rollback()

	// The delete trigger on urls keeps urls_fts in step.
	for _, stmt := range []string{`DELETE FROM urls`, `DELETE FROM sites`} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("flush store: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit flush: %w", err)
	}
	return nil
}
