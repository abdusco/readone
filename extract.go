package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/sourcegraph/conc/pool"
	xhtml "golang.org/x/net/html"
)

// FetchAndExtract fetches rawURL and runs server-side Readability extraction,
// for the UI's "import by URL" box (no browser/userscript involved). Unlike
// the userscript's browser-side import, images aren't already sitting in the
// visitor's cache — so we fetch them ourselves here, same as the userscript
// does with GM_xmlhttpRequest, and bundle them the same way (a zip with
// map.json alongside the images) so EPUB export and the reader page don't
// depend on the origin site being reachable a second time. ctx is honored by
// both the page fetch and every image fetch, so the caller's cancellation
// (e.g. the client disconnecting) stops in-flight requests instead of
// leaking them.
func FetchAndExtract(ctx context.Context, rawURL string) (Article, error) {
	pageURL, err := nurl.Parse(rawURL)
	if err != nil {
		return Article{}, err
	}

	art, err := readability.FromURL(rawURL, 30*time.Second, func(r *http.Request) {
		*r = *r.WithContext(ctx)
	})
	if err != nil {
		return Article{}, err
	}

	var buf bytes.Buffer
	if err := art.RenderHTML(&buf); err != nil {
		return Article{}, err
	}
	contentHTML := buf.String()

	return Article{
		Title:       art.Title(),
		Byline:      art.Byline(),
		SiteName:    art.SiteName(),
		URL:         pageURL.String(),
		ContentHTML: contentHTML,
		Assets:      downloadImages(ctx, extractImageURLs(contentHTML)),
	}, nil
}

// extractImageURLs collects the distinct absolute http(s) <img src> values in
// contentHTML, in document order. RenderHTML already resolves relative image
// URLs to absolute ones, so there's no base-URL handling to do here.
func extractImageURLs(contentHTML string) []string {
	doc, err := xhtml.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var urls []string
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key == "src" && strings.HasPrefix(attr.Val, "http") && !seen[attr.Val] {
					seen[attr.Val] = true
					urls = append(urls, attr.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return urls
}

const (
	imageFetchTimeout     = 15 * time.Second
	imageFetchMaxBytes    = 20 << 20 // 20MB
	imageFetchConcurrency = 4
)

type fetchedImage struct {
	url  string
	data []byte
	mime string
}

// downloadImages best-effort fetches each of urls (in parallel, bounded by
// imageFetchConcurrency) and bundles the ones that succeed into a zip
// (images/0.jpg, images/1.png, ...) alongside a map.json recording which
// original URL each entry came from. Failures — timeout, non-2xx, oversized
// body, a response whose sniffed content isn't actually an image (some
// sites 200 an HTML error/login page for a dead image URL), or ctx being
// canceled — are skipped silently, same graceful-degradation philosophy as
// the userscript's client-side fetch. Returns nil if no images were
// downloaded.
func downloadImages(ctx context.Context, urls []string) []byte {
	if len(urls) == 0 {
		return nil
	}

	client := &http.Client{Timeout: imageFetchTimeout}
	p := pool.NewWithResults[*fetchedImage]().WithMaxGoroutines(imageFetchConcurrency)
	for _, u := range urls {
		p.Go(func() *fetchedImage {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, imageFetchMaxBytes))
			if err != nil || len(data) == 0 {
				return nil
			}
			mime := http.DetectContentType(data)
			if !strings.HasPrefix(mime, "image/") {
				return nil
			}
			return &fetchedImage{url: u, data: data, mime: mime}
		})
	}
	fetched := p.Wait()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	assetMap := make(map[string]string)
	count := 0
	for _, img := range fetched {
		if img == nil {
			continue
		}
		name := fmt.Sprintf("images/%d%s", count, extForMime(img.mime))
		w, err := zw.Create(name)
		if err != nil {
			continue
		}
		if _, err := w.Write(img.data); err != nil {
			continue
		}
		assetMap[img.url] = name
		count++
	}
	if count == 0 {
		zw.Close()
		return nil
	}

	mapJSON, err := json.Marshal(assetMap)
	if err == nil {
		if w, err := zw.Create("map.json"); err == nil {
			w.Write(mapJSON)
		}
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}
