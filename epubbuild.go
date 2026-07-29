package main

import (
	"archive/zip"
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
		body, err := embedImages(book, a.ContentHTML, a.Assets, i)
		if err != nil {
			return nil, fmt.Errorf("article %d (%s): %w", a.ID, a.Title, err)
		}

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

// embedImages walks contentHTML and embeds every <img> into the EPUB,
// rewriting its src to the internal path go-epub returns, then re-serializes
// the HTML. Images the userscript already bundled into assetsZip (referenced
// by relative path, e.g. "images/0.jpg") are read straight from the zip;
// anything else falls back to fetching the remote URL directly, which is the
// only option for articles imported by URL (no browser involved).
func embedImages(book *epub.Epub, contentHTML string, assetsZip []byte, articleIdx int) (string, error) {
	var zr *zip.Reader
	if len(assetsZip) > 0 {
		if r, err := zip.NewReader(bytes.NewReader(assetsZip), int64(len(assetsZip))); err == nil {
			zr = r
		}
	}

	doc, err := xhtml.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return "", err
	}

	imgIdx := 0
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for i, attr := range n.Attr {
				if attr.Key != "src" {
					continue
				}
				internalPath, ok := embedOneImage(book, zr, attr.Val, articleIdx, imgIdx)
				if !ok {
					continue
				}
				imgIdx++
				n.Attr[i].Val = internalPath
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	body := findNode(doc, "body")
	if body == nil {
		return contentHTML, nil
	}

	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&buf, c); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}

// embedOneImage embeds a single image, preferring the bundled zip asset (if
// any) over a live fetch of a remote URL. Failures are non-fatal: the caller
// leaves the original src in place rather than aborting the whole export.
func embedOneImage(book *epub.Epub, zr *zip.Reader, src string, articleIdx, imgIdx int) (string, bool) {
	if zr != nil && !strings.HasPrefix(src, "http") {
		data, ok := readZipEntry(zr, src)
		if !ok {
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

// rewriteAssetPaths rewrites <img src="images/0.jpg">-style relative paths
// (left in place by the userscript's zip-asset bundling) into an absolute
// URL the browser can actually fetch, for rendering the article outside of
// EPUB export (the reader page, currently). Those relative paths only ever
// resolved to anything inside the zip go-epub embeds them from — on their
// own they're not servable, which is why images broke on the reader page
// once asset bundling shipped.
func rewriteAssetPaths(contentHTML string, articleID int64) string {
	doc, err := xhtml.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return contentHTML
	}

	prefix := fmt.Sprintf("/articles/%d/assets/", articleID)
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for i, attr := range n.Attr {
				if attr.Key == "src" && !strings.HasPrefix(attr.Val, "http") && !strings.HasPrefix(attr.Val, "/") && !strings.HasPrefix(attr.Val, "data:") {
					n.Attr[i].Val = prefix + attr.Val
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	body := findNode(doc, "body")
	if body == nil {
		return contentHTML
	}
	var buf bytes.Buffer
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&buf, c); err != nil {
			return contentHTML
		}
	}
	return buf.String()
}

func readZipEntry(zr *zip.Reader, name string) ([]byte, bool) {
	name = strings.TrimPrefix(name, "./")
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, false
		}
		return data, true
	}
	return nil, false
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
