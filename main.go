package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

//go:embed static
var staticFS embed.FS

var (
	indexHTML []byte
	readerTpl *template.Template
)

type server struct {
	db *sql.DB
}

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data.db"
	}
	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	s := &server{db: db}

	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true,
		LogURI:    true,
		LogMethod: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			log.Printf("%s %s %d", v.Method, v.URI, v.Status)
			return nil
		},
	}))
	e.Use(middleware.Recover())

	// DEBUG=1 serves templates/assets straight from disk so edits show up on
	// refresh instead of requiring a rebuild.
	var content fs.FS
	if os.Getenv("DEBUG") == "1" {
		content = os.DirFS("static")
	} else {
		content, err = fs.Sub(staticFS, "static")
		if err != nil {
			log.Fatalf("sub static fs: %v", err)
		}
	}

	if indexHTML, err = fs.ReadFile(content, "templates/index.html"); err != nil {
		log.Fatalf("read index.html: %v", err)
	}
	readerHTMLSrc, err := fs.ReadFile(content, "templates/reader.html")
	if err != nil {
		log.Fatalf("read reader.html: %v", err)
	}
	readerTpl = template.Must(template.New("reader").Parse(string(readerHTMLSrc)))

	assetsFS, err := fs.Sub(content, "assets")
	if err != nil {
		log.Fatalf("sub assets fs: %v", err)
	}

	e.GET("/", s.handleIndex)
	e.GET("/articles/:id", s.handleReaderPage)
	e.GET("/articles/:id/assets/*", s.handleArticleAsset)
	e.POST("/articles/epub", s.handleEPUB)
	e.GET("/readone.user.js", serveUserscript)
	e.GET("/robots.txt", handleRobots)
	e.StaticFS("/assets", assetsFS)

	// The userscript runs on whatever article page the user is reading, so
	// its origin can't be known in advance — allow any origin to call these.
	api := e.Group("/api")
	api.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost},
		AllowHeaders: []string{echo.HeaderContentType},
	}))
	api.GET("/articles", s.handleListArticles)
	api.POST("/articles", s.handleAPIImport)
	api.POST("/articles/import-url", s.handleImportURL)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	e.Logger.Fatal(e.Start(":" + port))
}

// This is a personal read-it-later tool, not something meant to be indexed —
// block every crawler rather than trying to enumerate specific ones.
func handleRobots(c echo.Context) error {
	return c.String(http.StatusOK, "User-agent: *\nDisallow: /\n")
}

func (s *server) handleIndex(c echo.Context) error {
	return c.Blob(http.StatusOK, "text/html; charset=utf-8", indexHTML)
}

func (s *server) handleReaderPage(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid article id")
	}
	articles, err := getArticlesByIDs(s.db, []int64{id})
	if err != nil {
		return err
	}
	if len(articles) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "article not found")
	}
	a := articles[0]

	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return readerTpl.Execute(c.Response(), map[string]any{
		"Title":    a.Title,
		"Byline":   a.Byline,
		"SiteName": a.SiteName,
		"URL":      a.URL,
		"Content":  template.HTML(rewriteAssetPaths(a.ContentHTML, a.ID)),
	})
}

// handleArticleAsset serves a single file out of an article's bundled assets
// zip (see rewriteAssetPaths), e.g. GET /articles/2/assets/images/0.jpg.
func (s *server) handleArticleAsset(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid article id")
	}
	name := c.Param("*")

	articles, err := getArticlesByIDs(s.db, []int64{id})
	if err != nil {
		return err
	}
	if len(articles) == 0 || len(articles[0].Assets) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}

	zr, err := zip.NewReader(bytes.NewReader(articles[0].Assets), int64(len(articles[0].Assets)))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "corrupt assets archive")
	}
	data, ok := readZipEntry(zr, name)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	return c.Blob(http.StatusOK, http.DetectContentType(data), data)
}

func (s *server) handleListArticles(c echo.Context) error {
	articles, err := listArticles(s.db)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, articles)
}

type importURLRequest struct {
	URL string `json:"url"`
}

func (s *server) handleImportURL(c echo.Context) error {
	var req importURLRequest
	if err := c.Bind(&req); err != nil || req.URL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "url is required")
	}
	art, err := FetchAndExtract(req.URL)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("extract failed: %v", err))
	}
	id, err := insertArticle(s.db, art)
	if err != nil {
		return err
	}
	art.ID = id
	return c.JSON(http.StatusOK, art)
}

type apiImportRequest struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Byline      string `json:"byline"`
	SiteName    string `json:"siteName"`
	ContentHTML string `json:"contentHtml"`
}

// handleAPIImport accepts multipart/form-data: a "metadata" JSON field plus
// an optional "assets" zip file (images the userscript already downloaded
// browser-side, so EPUB export doesn't depend on the origin site being
// reachable from the server later).
func (s *server) handleAPIImport(c echo.Context) error {
	var req apiImportRequest
	if err := json.Unmarshal([]byte(c.FormValue("metadata")), &req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid metadata field")
	}
	if req.URL == "" || req.ContentHTML == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "url and contentHtml are required")
	}

	var assets []byte
	if fh, err := c.FormFile("assets"); err == nil {
		f, err := fh.Open()
		if err != nil {
			return err
		}
		defer f.Close()
		if assets, err = io.ReadAll(f); err != nil {
			return err
		}
	}

	id, err := insertArticle(s.db, Article{
		Title:       req.Title,
		Byline:      req.Byline,
		SiteName:    req.SiteName,
		URL:         req.URL,
		ContentHTML: req.ContentHTML,
		Assets:      assets,
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"id": id})
}

type epubRequest struct {
	IDs []int64 `json:"ids"`
}

func (s *server) handleEPUB(c echo.Context) error {
	var req epubRequest
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "select at least one article")
	}

	articles, err := getArticlesByIDs(s.db, req.IDs)
	if err != nil {
		return err
	}
	if len(articles) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "no matching articles")
	}

	book, err := BuildEPUB(articles)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("build epub: %v", err))
	}

	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="articles.epub"`)
	c.Response().Header().Set(echo.HeaderContentType, "application/epub+zip")
	c.Response().WriteHeader(http.StatusOK)
	_, err = book.WriteTo(c.Response())
	return err
}
