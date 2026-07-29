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

## Development

```sh
go run .
```

## Releases

Pushing a `v*` tag (e.g. `v1.2.3`) triggers `.github/workflows/release.yml`, which builds and publishes a multi-arch Docker image to `ghcr.io/abdusco/readone`.
