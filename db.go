package main

import (
	"database/sql"
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
	// Assets is an optional zip archive (built client-side by the userscript)
	// containing the article's images, referenced from ContentHTML by
	// relative path (e.g. "images/0.jpg") instead of remote URL. This lets
	// EPUB export embed images without depending on the origin site's
	// server being reachable/unblocked at export time.
	Assets    []byte    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
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

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func insertArticle(db *sql.DB, a Article) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO articles (title, byline, site_name, url, content_html, assets) VALUES (?, ?, ?, ?, ?, ?)`,
		a.Title, a.Byline, a.SiteName, a.URL, a.ContentHTML, a.Assets,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// listArticles deliberately omits the assets blob — it can be large and the
// list view never needs it.
func listArticles(db *sql.DB) ([]Article, error) {
	rows, err := db.Query(`SELECT id, title, byline, site_name, url, content_html, created_at FROM articles ORDER BY created_at DESC`)
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

// getArticlesByIDs includes assets since it's used for EPUB building.
func getArticlesByIDs(db *sql.DB, ids []int64) ([]Article, error) {
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

	rows, err := db.Query(query, args...)
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
