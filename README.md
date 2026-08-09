[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)

# Bitcoin Historical Events API

A Go-based, **read-only** API server for historical Bitcoin events. It supports multiple
languages for event data.

## Project Overview

-   **API Server (`main.go`)**: A Fiber-based Go application that serves event data.
    -   Supports language selection via the `lang` query parameter (e.g., `lang=en`, `lang=ru`).
    -   Reads `events_en.db` (English, default) and `events_ru.db` (Russian).
    -   Requires an API key (`X-API-KEY` header) for authentication.
    -   Uses environment variables for configuration (API keys, database paths, listen address).
-   **Databases**: **not stored in this repository.** The canonical databases are authored and
    validated elsewhere and shipped to the server as an immutable artifact
    (`/srv/bitcal/data/current/events_{ru,en}.db`, mode `0444`, in a directory the service user
    cannot write). This service opens them read-only and never writes, migrates, or creates
    indexes, triggers or FTS tables — the artifact ships with its own.

## Key Features

-   Paginated event listings.
-   Filtering events by month/day.
-   Fetching individual events by ID.
-   Listing unique event tags and their counts.
-   Fetching events by specific tags.
-   Language support for event content (English and Russian).
-   Rate limiting and API key authentication.
-   Full-text search functionality on event titles, descriptions, and tags.

## API Endpoints

A brief overview of the main endpoints. For detailed information, see `docs/APIDocumentation.md`.

-   `GET /api/events`: Lists all events with pagination.
-   `GET /api/events/:id`: Fetches a single event by its ID.
-   `GET /api/search?q={query}`: Performs a full-text search on events.
-   `GET /api/tags`: Retrieves a list of all unique tags and their usage counts.
-   `GET /api/events/tags/:tag`: Gets events associated with a specific tag.

## Documentation

Detailed documentation for the API, database schema, and deployment can be found in the `/docs` directory:
-   `docs/APIDocumentation.md`
-   `docs/DatabaseDocumentation.md`
-   `docs/Deployment.md`

## Setup and Running

```sh
make build     # CGO_ENABLED=1 go build -tags fts5 …
make test
```

`-tags fts5` is mandatory: the SQLite driver does not compile FTS5 in by default, and
without it every search fails at runtime with `no such module: fts5`. A build without the
tag is refused outright — see `fts5_required.go`. Build on Ubuntu/glibc or in a matching
container (`make build-ubuntu`) — CGO ties the binary to its libc, so a Mac- or
Alpine-built binary will not start on the server.

## Tests

The suite is in `tests/` and is black-box: it builds this binary, stages a database fixture
exactly as a release is staged (mode `0444`, in a `0555` directory), starts the service
against it and drives it over HTTP. So `make test` also exercises the build, the read-only
open, and the JSON contract the Telegram bot depends on.

Deployment is a native systemd service, not Docker: see `deploy/bitcal-api.service`, whose
comment header also records the manual release steps for a new database artifact.

## Environment Variables

The API server uses the following environment variables:

-   `API_KEYS`: (Required) A comma-separated list of secret keys for API authentication. For example: `key1,key2,anotherkey`
-   `DB_PATH_EN`: (Required) Path to the English SQLite database. No default — startup fails if unset.
-   `DB_PATH_RU`: (Required) Path to the Russian SQLite database. No default — startup fails if unset.
-   `LISTEN_ADDR`: Address to bind. Defaults to `127.0.0.1:3000`.
-   `CORS_ALLOWED_ORIGINS`: Comma-separated list of allowed origins for CORS. Defaults to `http://localhost:3000`.

## Health

`GET /health` is unauthenticated and reports, per language, the symlink-resolved path of the
database file this process has open, its SHA-256 and its row count. The hash is computed once
at startup, so it describes the inode actually being served rather than whatever the `current`
symlink points at when you ask.

## Testing

API will be publicly available in Q3 2026, if you want to test API now, DM [@Tony](https://njump.me/npub10awzknjg5r5lajnr53438ndcyjylgqsrnrtq5grs495v42qc6awsj45ys7) on Nostr – I'll be happy to share a key with you.

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)