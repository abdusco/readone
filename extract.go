package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	nurl "net/url"
	"slices"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/samber/lo"
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

	parsed, err := transformHTML(buf.String(), stripEmptyListsTransform)
	if err != nil {
		return Article{}, err
	}
	imageURLs := extractImageURLs(parsed, pageURL)
	contentHTML, err := renderBody(parsed)
	if err != nil {
		return Article{}, err
	}

	return Article{
		Title:       art.Title(),
		Byline:      art.Byline(),
		SiteName:    art.SiteName(),
		URL:         pageURL.String(),
		ContentHTML: contentHTML,
		Assets:      downloadImages(ctx, imageURLs),
	}, nil
}

// extractImageURLs collects the distinct http(s) <img src> values under doc,
// in document order, resolving any relative src against base (the page's own
// URL) the same way a browser would. data: URIs and anything that isn't (or
// doesn't resolve to) an http(s) URL are skipped.
func extractImageURLs(doc *xhtml.Node, base *nurl.URL) []string {
	seen := make(map[string]bool)
	var urls []string
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for _, attr := range n.Attr {
				if attr.Key != "src" || strings.HasPrefix(attr.Val, "data:") {
					continue
				}
				resolved, ok := resolveImageURL(base, attr.Val)
				if !ok || seen[resolved] {
					continue
				}
				seen[resolved] = true
				urls = append(urls, resolved)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return urls
}

// resolveImageURL parses ref and, if it's not already absolute, resolves it
// against base. Reports false if ref doesn't parse, is relative with no
// base to resolve against, or resolves to something other than http(s)
// (e.g. javascript:, mailto:).
func resolveImageURL(base *nurl.URL, ref string) (string, bool) {
	u, err := nurl.Parse(ref)
	if err != nil {
		return "", false
	}
	resolved := u
	if !u.IsAbs() {
		if base == nil {
			return "", false
		}
		resolved = base.ResolveReference(u)
	}
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", false
	}
	return resolved.String(), true
}

// mediaTags are element names whose presence alone (even with no text)
// counts as meaningful content for hasMeaningfulContent.
var mediaTags = map[string]bool{"img": true, "picture": true, "video": true, "audio": true, "iframe": true, "svg": true}

// stripEmptyLists removes <li> elements with no real content and any
// <ul>/<ol> left with no <li> children as a result. Byproducts of
// Readability's own boilerplate cleanup: it strips a list item's children
// without removing the item, and strips every item without removing the
// now-empty wrapper.
func stripEmptyLists(contentHTML string) string {
	return transformHTMLString(contentHTML, stripEmptyListsTransform)
}

// stripEmptyListsTransform is stripEmptyLists as a transformHTML step.
// Processed bottom-up so emptiness cascades correctly (an <li> containing
// only an empty nested list is itself empty, which can in turn empty its
// parent list).
func stripEmptyListsTransform(doc *xhtml.Node) {
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			walk(c)
			c = next
		}
		if n.Type != xhtml.ElementNode || n.Parent == nil {
			return
		}
		switch n.Data {
		case "li":
			if !hasMeaningfulContent(n) {
				n.Parent.RemoveChild(n)
			}
		case "ul", "ol":
			if !hasElementChild(n, "li") {
				n.Parent.RemoveChild(n)
			}
		}
	}
	walk(doc)
}

// hasMeaningfulContent reports whether n's subtree has any non-whitespace
// text or a media element (which can be meaningful with no text at all).
func hasMeaningfulContent(n *xhtml.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case xhtml.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				return true
			}
		case xhtml.ElementNode:
			if mediaTags[c.Data] || hasMeaningfulContent(c) {
				return true
			}
		}
	}
	return false
}

// hasElementChild reports whether n has a direct child element named tag.
func hasElementChild(n *xhtml.Node, tag string) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == xhtml.ElementNode && c.Data == tag {
			return true
		}
	}
	return false
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

	fetched = lo.Without(fetched, nil)
	// conc/pool doesn't guarantee result order matches submission order;
	// sort by URL so the images/N.ext numbering is deterministic regardless
	// of which fetch happens to finish first.
	slices.SortFunc(fetched, func(a, b *fetchedImage) int {
		return cmp.Compare(a.url, b.url)
	})

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
