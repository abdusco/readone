package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"strings"

	epub "github.com/bmaupin/go-epub"
	xhtml "golang.org/x/net/html"
)

// BuildEPUB merges the given articles into a single EPUB, one chapter per
// article, embedding each article's remote images into the book so it reads
// offline.
func BuildEPUB(articles []Article) (io.WriterTo, error) {
	title := "Articles"
	if len(articles) == 1 {
		title = articles[0].Title
	} else {
		title = fmt.Sprintf("Articles (%d)", len(articles))
	}
	book := epub.NewEpub(title)

	for i, a := range articles {
		body, err := embedImages(book, a.ContentHTML)
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

// embedImages walks contentHTML, downloads every remote <img> into the EPUB
// via go-epub's AddImage, rewrites the src to the internal path it returns,
// and re-serializes the HTML.
func embedImages(book *epub.Epub, contentHTML string) (string, error) {
	doc, err := xhtml.Parse(strings.NewReader(contentHTML))
	if err != nil {
		return "", err
	}

	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "img" {
			for i, attr := range n.Attr {
				if attr.Key != "src" || !strings.HasPrefix(attr.Val, "http") {
					continue
				}
				internalPath, err := book.AddImage(attr.Val, "")
				if err != nil {
					// Skip images that fail to download rather than failing the whole export.
					continue
				}
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
