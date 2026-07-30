package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Minimal byte sequences that satisfy net/http's content-sniffing rules
// (http.DetectContentType only inspects a signature prefix, so these don't
// need to be structurally valid images).
var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n" + "rest of file doesn't matter for sniffing")
	jpegMagic = []byte("\xFF\xD8\xFF" + "rest of file doesn't matter for sniffing")
)

func TestDownloadImages(t *testing.T) {
	tests := []struct {
		name string
		// contents maps a request path to the body its handler serves. A nil
		// value serves a 404 instead, so a case can exercise "image URL is
		// dead" without a separate field.
		contents map[string][]byte
		assert   func(t *testing.T, actual []byte)
	}{
		{
			name:     "no urls",
			contents: map[string][]byte{},
			assert: func(t *testing.T, actual []byte) {
				assert.Nil(t, actual)
			},
		},
		{
			name: "happy path",
			contents: map[string][]byte{
				"/a.png":       pngMagic,
				"/b.jpg":       jpegMagic,
				"/missing.jpg": nil,
				"/empty.jpg":   {},
				"/fake.png":    []byte("<html>not an image</html>"),
			},
			assert: func(t *testing.T, actual []byte) {
				zr := requireZip(t, actual)
				assetMap := readAssetMap(zr)
				require.Len(t, assetMap, 2)
				assertZipContains(t, zr, map[string][]byte{
					"images/0.png": pngMagic,
					"images/1.jpg": jpegMagic,
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			urls := make([]string, 0, len(tt.contents))
			for path, body := range tt.contents {
				mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
					if body == nil {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Write(body)
				})
				urls = append(urls, path)
			}
			srv := httptest.NewServer(mux)
			defer srv.Close()
			for i, path := range urls {
				urls[i] = srv.URL + path
			}

			actual := downloadImages(context.Background(), urls)
			tt.assert(t, actual)
		})
	}
}

// requireZip parses actual as a zip, failing the test immediately if it
// isn't one.
func requireZip(t *testing.T, actual []byte) *zip.Reader {
	t.Helper()
	require.NotNil(t, actual)
	zr, err := zip.NewReader(bytes.NewReader(actual), int64(len(actual)))
	require.NoError(t, err)
	return zr
}

// assertZipContains asserts that for every zipPath -> content pair in want,
// zr has an entry at zipPath whose bytes equal content.
func assertZipContains(t *testing.T, zr *zip.Reader, want map[string][]byte) {
	t.Helper()
	for path, wantData := range want {
		data, ok := readZipEntry(zr, path)
		if !assert.True(t, ok, "zip missing entry %q", path) {
			continue
		}
		assert.Equal(t, wantData, data, "zip entry %q", path)
	}
}

func TestExtractImageURLs(t *testing.T) {
	base, err := url.Parse("https://example.com/articles/story")
	require.NoError(t, err)

	tests := []struct {
		name        string
		contentHTML string
		base        *url.URL
		want        []string
	}{
		{
			name:        "no images",
			contentHTML: `<p>just text</p>`,
			base:        base,
			want:        nil,
		},
		{
			name:        "single absolute image",
			contentHTML: `<img src="https://example.com/a.jpg">`,
			base:        base,
			want:        []string{"https://example.com/a.jpg"},
		},
		{
			name:        "duplicate src collapsed to one entry",
			contentHTML: `<img src="https://example.com/a.jpg"><img src="https://example.com/a.jpg">`,
			base:        base,
			want:        []string{"https://example.com/a.jpg"},
		},
		{
			name:        "root-relative src resolved against base",
			contentHTML: `<img src="/images/a.jpg">`,
			base:        base,
			want:        []string{"https://example.com/images/a.jpg"},
		},
		{
			name:        "document-relative src resolved against base's directory",
			contentHTML: `<img src="a.jpg">`,
			base:        base,
			want:        []string{"https://example.com/articles/a.jpg"},
		},
		{
			name:        "protocol-relative src resolved against base's scheme",
			contentHTML: `<img src="//cdn.example.com/a.jpg">`,
			base:        base,
			want:        []string{"https://cdn.example.com/a.jpg"},
		},
		{
			name:        "relative src with no base is skipped",
			contentHTML: `<img src="/a.jpg">`,
			base:        nil,
			want:        nil,
		},
		{
			name:        "data-URL images ignored",
			contentHTML: `<img src="data:image/png;base64,abc">`,
			base:        base,
			want:        nil,
		},
		{
			name:        "non-http(s) scheme ignored even when absolute",
			contentHTML: `<img src="javascript:alert(1)">`,
			base:        base,
			want:        nil,
		},
		{
			name:        "preserves document order across multiple images",
			contentHTML: `<div><img src="https://example.com/b.jpg"></div><img src="https://example.com/a.jpg">`,
			base:        base,
			want:        []string{"https://example.com/b.jpg", "https://example.com/a.jpg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractImageURLs(tt.contentHTML, tt.base))
		})
	}
}

func TestReadAssetMap(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string][]byte // zip entries to write; "map.json" is written verbatim if present
		want    map[string]string
	}{
		{
			name:    "no map.json in zip",
			entries: map[string][]byte{"images/0.png": pngMagic},
			want:    nil,
		},
		{
			name:    "invalid json",
			entries: map[string][]byte{"map.json": []byte("not json")},
			want:    nil,
		},
		{
			name:    "valid mapping",
			entries: map[string][]byte{"map.json": []byte(`{"https://example.com/a.jpg":"images/0.jpg"}`)},
			want:    map[string]string{"https://example.com/a.jpg": "images/0.jpg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for name, data := range tt.entries {
				w, err := zw.Create(name)
				require.NoError(t, err)
				_, err = w.Write(data)
				require.NoError(t, err)
			}
			require.NoError(t, zw.Close())

			zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			require.NoError(t, err)

			assert.Equal(t, tt.want, readAssetMap(zr))
		})
	}
}
