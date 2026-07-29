package main

import (
	"bytes"
	nurl "net/url"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
)

// FetchAndExtract fetches rawURL and runs server-side Readability extraction,
// for the UI's "import by URL" box (no browser/userscript involved).
func FetchAndExtract(rawURL string) (Article, error) {
	pageURL, err := nurl.Parse(rawURL)
	if err != nil {
		return Article{}, err
	}

	art, err := readability.FromURL(rawURL, 30*time.Second)
	if err != nil {
		return Article{}, err
	}

	var buf bytes.Buffer
	if err := art.RenderHTML(&buf); err != nil {
		return Article{}, err
	}

	return Article{
		Title:       art.Title(),
		Byline:      art.Byline(),
		SiteName:    art.SiteName(),
		URL:         pageURL.String(),
		ContentHTML: buf.String(),
	}, nil
}
