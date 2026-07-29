package main

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed userscript.js
var userscriptSource string

// serveUserscript serves the userscript with its SAVE_URL placeholder
// replaced by this server's own origin, so it's ready to use as soon as
// it's installed — no manual configuration step.
func serveUserscript(c echo.Context) error {
	// c.Scheme() checks X-Forwarded-Proto (among others), not just the
	// connection's own TLS state — needed when a reverse proxy terminates
	// TLS in front of the app, which otherwise only ever sees plain HTTP.
	origin := c.Scheme() + "://" + c.Request().Host

	script := strings.Replace(userscriptSource, "__SAVE_URL__", origin, 1)
	return c.Blob(http.StatusOK, "application/javascript; charset=utf-8", []byte(script))
}
