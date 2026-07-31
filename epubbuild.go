package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"

	epub "github.com/bmaupin/go-epub"
	xhtml "golang.org/x/net/html"
)

// BuildEPUB merges the given articles into a single EPUB, one chapter per
// article, embedding each article's images into the book so it reads offline.
func BuildEPUB(articles []Article) (io.WriterTo, error) {
	title := "Articles"
	if len(articles) == 1 {
		title = articles[0].Title
	} else {
		title = fmt.Sprintf("Articles (%d)", len(articles))
	}
	book := epub.NewEpub(title)

	for i, a := range articles {
		body := transformHTMLString(a.ContentHTML,
			resolveAssetPathsTransform(a),
			stripEmptyListsTransform,
			embedImagesTransform(book, a.Assets, i),
		)

		header := fmt.Sprintf(`<p><em>%s</em></p><p><a href="%s">%s</a></p>`,
			html.EscapeString(strings.Join(nonEmpty(a.Byline, a.SiteName), " · ")),
			html.EscapeString(a.URL), html.EscapeString(a.URL))

		if _, err := book.AddSection(header+body, a.Title, fmt.Sprintf("chap%d.xhtml", i), ""); err != nil {
			return nil, fmt.Errorf("article %d (%s): %w", a.ID, a.Title, err)
		}
	}

	return book, nil
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// transformHTML parses contentHTML once and runs each transform over the
// resulting tree in order, so a caller needing the tree itself afterward
// (e.g. to extract <img> URLs) doesn't have to parse contentHTML again.
func transformHTML(contentHTML string, transforms ...func(*xhtml.Node)) (*xhtml.Node, error) {
	doc, err := xhtml.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return nil, err
	}
	for _, t := range transforms {
		t(doc)
	}
	return doc, nil
}

// renderBody re-serializes doc's <body> children back to an HTML string.
func renderBody(doc *xhtml.Node) (string, error) {
	body := findNode(doc, "body")
	if body == nil {
		return "", fmt.Errorf("no <body> in document")
	}
	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&buf, c); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}

// transformHTMLString is transformHTML for callers that just want the
// resulting HTML string, falling back to contentHTML unchanged if parsing
// or re-rendering fails.
func transformHTMLString(contentHTML string, transforms ...func(*xhtml.Node)) string {
	doc, err := transformHTML(contentHTML, transforms...)
	if err != nil {
		return contentHTML
	}
	rendered, err := renderBody(doc)
	if err != nil {
		return contentHTML
	}
	return rendered
}

// walkImgSrc visits every <img> under doc, replacing its src with whatever
// rewrite returns when it reports a match.
func walkImgSrc(doc *xhtml.Node, rewrite func(src string) (string, bool)) {
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for i, attr := range n.Attr {
				if attr.Key != "src" {
					continue
				}
				if newSrc, ok := rewrite(attr.Val); ok {
					n.Attr[i].Val = newSrc
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// embedImagesTransform embeds every <img> into the EPUB, rewriting its src to
// the internal path go-epub returns. Images the userscript already bundled
// into assets (referenced by relative path, e.g. "images/0.jpg") are read
// straight from the zip; anything else falls back to fetching the remote URL
// directly, which is the only option for articles imported by URL (no
// browser involved).
func embedImagesTransform(book *epub.Epub, assets Assets, articleIdx int) func(*xhtml.Node) {
	imgIdx := 0
	return func(doc *xhtml.Node) {
		walkImgSrc(doc, func(src string) (string, bool) {
			internalPath, ok := embedOneImage(book, assets, src, articleIdx, imgIdx)
			if !ok {
				return "", false
			}
			imgIdx++
			return internalPath, true
		})
	}
}

// embedOneImage embeds a single image, preferring the bundled zip asset (if
// any) over a live fetch of a remote URL. Failures are non-fatal: the caller
// leaves the original src in place rather than aborting the whole export.
func embedOneImage(book *epub.Epub, assets Assets, src string, articleIdx, imgIdx int) (string, bool) {
	if !assets.Empty() && !strings.HasPrefix(src, "http") {
		data, err := assets.Entry(src)
		if err != nil {
			return "", false
		}
		mime := http.DetectContentType(data)
		filename := fmt.Sprintf("a%d_%d%s", articleIdx, imgIdx, extForMime(mime))
		dataURL := fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
		internalPath, err := book.AddImage(dataURL, filename)
		if err != nil {
			return "", false
		}
		return internalPath, true
	}

	if !strings.HasPrefix(src, "http") {
		return "", false
	}
	internalPath, err := book.AddImage(src, "")
	if err != nil {
		return "", false
	}
	return internalPath, true
}

// rewriteAssetPathsTransform rewrites <img src="images/0.jpg">-style relative
// paths (left in place by the userscript's zip-asset bundling) into an
// absolute URL the browser can actually fetch, for rendering the article
// outside of EPUB export (the reader page, currently). Those relative paths
// only ever resolved to anything inside the zip go-epub embeds them from —
// on their own they're not servable, which is why images broke on the reader
// page once asset bundling shipped.
func rewriteAssetPathsTransform(articleID int64) func(*xhtml.Node) {
	prefix := fmt.Sprintf("/articles/%d/assets/", articleID)
	return func(doc *xhtml.Node) {
		walkImgSrc(doc, func(src string) (string, bool) {
			if strings.HasPrefix(src, "http") || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "data:") {
				return "", false
			}
			return prefix + src, true
		})
	}
}

// resolveAssetPathsTransform rewrites any <img src> that a.Assets' map.json
// knows about to its zip-relative path, so rewriteAssetPathsTransform
// (reader page) / embedImagesTransform (EPUB) can pick it up exactly as if
// the userscript had baked the path in itself.
func resolveAssetPathsTransform(a Article) func(*xhtml.Node) {
	return func(doc *xhtml.Node) {
		assetMap := a.Assets.AssetMap()
		if len(assetMap) == 0 {
			return
		}
		walkImgSrc(doc, func(src string) (string, bool) {
			p, ok := assetMap[src]
			return p, ok
		})
	}
}

func extForMime(mime string) string {
	switch {
	case strings.Contains(mime, "jpeg"):
		return ".jpg"
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "svg"):
		return ".svg"
	default:
		return ".img"
	}
}

func findNode(n *xhtml.Node, tag string) *xhtml.Node {
	if n.Type == xhtml.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findNode(c, tag); found != nil {
			return found
		}
	}
	return nil
}
