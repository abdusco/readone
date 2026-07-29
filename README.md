<p align="center">
  <img src="assets/logo.svg" width="120" alt="readone logo">
</p>

# readone

A minimal read-it-later server: import articles via URL or a userscript, read them in a clean reader view, and export them as EPUB.

## Running

```sh
docker compose up
```

The app listens on `:8080` and stores its SQLite database in the `readone-data` volume.

### Configuration

| Env var   | Default            | Description                |
| --------- | ------------------- | -------------------------- |
| `PORT`    | `8080`               | HTTP listen port           |
| `DB_PATH` | `/app/data/data.db`  | Path to the SQLite database |

## Userscript

Install the Tampermonkey/Greasemonkey script served at `/readone.user.js` (the
server rewrites its save URL to point at itself, so no manual setup is
needed). It adds a draggable "Reader" tab to every page that:

- Extracts the article with Readability, fixing up lazy-loaded, background,
  and `<noscript>` images first so they survive extraction.
- Lets you view it in a reader overlay, copy/download it as Markdown, or
  save it to this server.
- On save, downloads every remote `<img>` in the extracted content
  **from the browser** (via `GM_xmlhttpRequest`, using the page's own
  cookies/UA/referrer to get past hotlinking and bot-blocking) and bundles
  them into a zip alongside the article. This works even for images the
  server itself couldn't fetch directly.

The server stores that zip as-is next to the article. Images referenced by
it are served from `/articles/:id/assets/*` for the reader view, and are
embedded directly when exporting to EPUB. Images that fail to download stay
as plain remote URLs — nothing breaks, they just aren't bundled.

## Development

```sh
go run .
```

## Releases

Pushing a `v*` tag (e.g. `v1.2.3`) triggers `.github/workflows/release.yml`, which builds and publishes a multi-arch Docker image to `ghcr.io/abdusco/readone`.
