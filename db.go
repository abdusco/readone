package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Article struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Byline      string    `json:"byline"`
	SiteName    string    `json:"siteName"`
	URL         string    `json:"url"`
	ContentHTML string    `json:"contentHtml"`
	Assets      Assets    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Assets is an optional zip archive (built client-side by the userscript, or
// server-side by downloadImages) containing an article's images, referenced
// from ContentHTML by relative path (e.g. "images/0.jpg") instead of remote
// URL. This lets EPUB export and the reader page render images without
// depending on the origin site's server being reachable later.
type Assets struct {
	data []byte
	zr   *zip.Reader // nil iff data is empty; parsed once at construction
}

// NewAssets wraps data as a zip archive of assets, parsing it immediately so
// corruption is caught at construction time rather than resurfacing later
// when something tries to read an entry out of it. Empty/nil data is valid —
// it just means "no assets".
func NewAssets(data []byte) (Assets, error) {
	if len(data) == 0 {
		return Assets{}, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Assets{}, fmt.Errorf("invalid assets zip: %w", err)
	}
	return Assets{data: data, zr: zr}, nil
}

// Empty reports whether there are no assets at all.
func (a Assets) Empty() bool { return len(a.data) == 0 }

// Entry extracts a single file from the assets zip by path (e.g. "images/0.jpg").
func (a Assets) Entry(path string) ([]byte, error) {
	if a.zr == nil {
		return nil, fmt.Errorf("asset %q not found", path)
	}
	path = strings.TrimPrefix(path, "./")
	for _, f := range a.zr.File {
		if f.Name != path {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, fmt.Errorf("asset %q not found", path)
}

// AssetMap reads the optional map.json entry (original image URL -> in-zip
// path). Returns nil if there's no mapping — covers zips built before this
// existed, and articles with no assets at all.
func (a Assets) AssetMap() map[string]string {
	data, err := a.Entry("map.json")
	if err != nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// Value implements driver.Valuer so Assets can be written directly as a BLOB.
func (a Assets) Value() (driver.Value, error) {
	if len(a.data) == 0 {
		return nil, nil
	}
	return a.data, nil
}

// Scan implements sql.Scanner, re-parsing the zip so corruption in the
// database surfaces immediately as a query error instead of only when an
// asset is later requested.
func (a *Assets) Scan(src any) error {
	if src == nil {
		*a = Assets{}
		return nil
	}
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("Assets.Scan: unsupported type %T", src)
	}
	parsed, err := NewAssets(append([]byte(nil), b...))
	if err != nil {
		return fmt.Errorf("Assets.Scan: %w", err)
	}
	*a = parsed
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS articles (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	title        TEXT NOT NULL,
	byline       TEXT,
	site_name    TEXT,
	url          TEXT NOT NULL,
	content_html TEXT NOT NULL,
	assets       BLOB,
	created_at   DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
`

// Repo wraps the sqlite connection and provides article persistence.
type Repo struct {
	db *sql.DB
}

// newRepo opens (creating if necessary) the sqlite database at path and
// returns a Repo backed by it.
func newRepo(path string) (*Repo, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// De-dupe any rows saved before the URL uniqueness constraint existed,
	// keeping the most recently inserted row per URL, so the index below
	// can always be created.
	if _, err := db.Exec(`DELETE FROM articles WHERE id NOT IN (SELECT MAX(id) FROM articles GROUP BY url)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("de-dupe articles by url: %w", err)
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_articles_url ON articles(url)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create url unique index: %w", err)
	}
	return &Repo{db: db}, nil
}

// Close closes the underlying database connection.
func (r *Repo) Close() error {
	return r.db.Close()
}

// InsertArticle upserts by URL: saving an already-saved URL again replaces
// its title/byline/content/assets and bumps created_at, rather than creating
// a duplicate row.
func (r *Repo) InsertArticle(a Article) (int64, error) {
	_, err := r.db.Exec(
		`INSERT INTO articles (title, byline, site_name, url, content_html, assets)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(url) DO UPDATE SET
		   title        = excluded.title,
		   byline       = excluded.byline,
		   site_name    = excluded.site_name,
		   content_html = excluded.content_html,
		   assets       = excluded.assets,
		   created_at   = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		a.Title, a.Byline, a.SiteName, a.URL, a.ContentHTML, a.Assets,
	)
	if err != nil {
		return 0, err
	}
	// LastInsertId() isn't reliably updated on the DO UPDATE branch of an
	// upsert, so look the row up by its unique URL instead.
	var id int64
	if err := r.db.QueryRow(`SELECT id FROM articles WHERE url = ?`, a.URL).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// ListArticles deliberately omits the assets blob — it can be large and the
// list view never needs it.
func (r *Repo) ListArticles() ([]Article, error) {
	rows, err := r.db.Query(`SELECT id, title, byline, site_name, url, content_html, created_at FROM articles ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.Title, &a.Byline, &a.SiteName, &a.URL, &a.ContentHTML, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteArticle removes a single article by id.
func (r *Repo) DeleteArticle(id int64) error {
	_, err := r.db.Exec(`DELETE FROM articles WHERE id = ?`, id)
	return err
}

// GetArticlesByIDs includes assets since it's used for EPUB building.
func (r *Repo) GetArticlesByIDs(ids []int64) ([]Article, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := `SELECT id, title, byline, site_name, url, content_html, assets, created_at FROM articles WHERE id IN (` +
		placeholders + `) ORDER BY created_at DESC`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.Title, &a.Byline, &a.SiteName, &a.URL, &a.ContentHTML, &a.Assets, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
