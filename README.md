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

Refer to `docs/Deployment.md` for instructions on building and running the API using Docker.

## Environment Variables

The API server uses the following environment variables:

-   `API_KEYS`: (Required) A comma-separated list of secret keys for API authentication. For example: `key1,key2,anotherkey`
-   `DB_PATH_EN`: Path to the English SQLite database. Defaults to `./data/events.db`.
-   `DB_PATH_RU`: Path to the Russian SQLite database. Defaults to `./data/events_ru.db`.
-   `PORT`: Port for the API server. Defaults to `3000`.
-   `CORS_ALLOWED_ORIGINS`: Comma-separated list of allowed origins for CORS. Defaults to `http://localhost:3000`.

## Testing

API will be publicly available in Q3 2025, if you want to test API now, DM [@Tony](https://njump.me/npub10awzknjg5r5lajnr53438ndcyjylgqsrnrtq5grs495v42qc6awsj45ys7) on Nostr – I'll be happy to share a key with you.

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)