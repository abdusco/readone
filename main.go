package main

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

//go:embed templates/index.html
var indexHTML []byte

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

	e.GET("/", s.handleIndex)
	e.POST("/articles/epub", s.handleEPUB)
	e.GET("/readone.user.js", serveUserscript)

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

func (s *server) handleIndex(c echo.Context) error {
	return c.Blob(http.StatusOK, "text/html; charset=utf-8", indexHTML)
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

func (s *server) handleAPIImport(c echo.Context) error {
	var req apiImportRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if req.URL == "" || req.ContentHTML == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "url and contentHtml are required")
	}
	id, err := insertArticle(s.db, Article{
		Title:       req.Title,
		Byline:      req.Byline,
		SiteName:    req.SiteName,
		URL:         req.URL,
		ContentHTML: req.ContentHTML,
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
