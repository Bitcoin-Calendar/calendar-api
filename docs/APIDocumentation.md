[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)

# Bitcoin Historical Events API Documentation

This document provides details for interacting with the Bitcoin Historical Events API.

## Base URL

The service binds `127.0.0.1:3000` by default (configurable with `LISTEN_ADDR`), so the base
URL for all API endpoints is:

`http://localhost:3000/api`

There is no public vhost in front of it yet. Older documentation and the frozen Nostr bot
refer to a host at `213.176.74.147:3001` — that is a different, older deployment, not this one.

## Read-only

**This API only reads.** It serves a database artifact that is authored, validated and
published elsewhere, and it opens that artifact read-only. There are no `POST`, `PUT` or
`DELETE` endpoints, no `/migrate`, and no schema management at startup — earlier versions had
all of these and they have been removed. `Access-Control-Allow-Methods` is `GET,HEAD,OPTIONS`.

## Authentication

The API requires an API key to be passed in the `X-API-KEY` header for all endpoints under `/api`. The server can be configured with one or more comma-separated keys via the `API_KEYS` environment variable.

### CORS

Browser-based clients must comply with Cross-Origin Resource Sharing (CORS) rules. The server automatically adds the appropriate `Access-Control-*` headers when the request's `Origin` value is included in `CORS_ALLOWED_ORIGINS` (comma-separated list, defaults to `http://localhost:3000`).  Pre-flight `OPTIONS` requests are handled transparently and receive a `204 No Content` response.  Non-browser tools (curl, bots) that do not send the `Origin` header remain unaffected.

## Rate Limiting

Rate limiting is applied per IP address. The current limit is 100 requests per minute.

## Overview of Endpoints

The API provides the following main functionalities:

*   **`/events`**: Retrieve a paginated list of all events, with powerful filtering by date (year, month, day, or combinations) and language.
*   **`/events/:id`**: Fetch a single event by its unique ID.
*   **`/search`**: Perform a full-text search across event titles, descriptions, and tags.
*   **`/tags`**: Get a list of all unique event tags and their usage counts.
*   **`/events/tags/:tag`**: Retrieve a paginated list of events associated with a specific tag.
*   **`/health`**: Report which database artifact the service has open. **Not** under `/api`, and needs no API key.

Detailed information for each endpoint is provided below.

## The event object

Every endpoint that returns events returns objects of this shape. Four points are worth
reading before writing a client, because all four have changed:

| Field | Type | Notes |
|---|---|---|
| `id` | integer | **Independent per language.** RU `100` and EN `100` are unrelated events. Do not use it as a cross-language key — use `url_path`. |
| `date` | string | `YYYY-MM-DD`, e.g. `"1881-09-29"`. **Not** an RFC 3339 timestamp; earlier versions emitted `"1881-09-29T00:00:00Z"`, inventing a time and a timezone the data never had. The range starts in 1881, before the Unix epoch — parse accordingly. |
| `title` | string | |
| `description` | string | |
| `tags` | string | A **JSON array encoded as a string**, e.g. `"[\"bitcoin\",\"whitepaper\"]"`. Parse it; it is not a list. |
| `media` | string \| null | A JSON array encoded as a string, or `null` when the event has no media. Never `""` and never `"[]"` — absence is exactly one value. |
| `references` | string \| null | Same encoding and same null rule as `media`. |
| `url_path` | string | `/<date>/<slug>/`, e.g. `"/2013-08-09/hal-finneys-last-post/"`. The site's page path and the cross-language join key. **Note the leading slash** — joining it onto a base URL with another `/` yields a double slash. |
| `created_at` | string \| null | RFC 3339, or `null`. Bookkeeping about the row, not about the event; most rows have no value. |
| `updated_at` | string \| null | Same. |

```json
{
  "id": 267,
  "date": "2013-08-09",
  "title": "✍️ Последний пост Хэла Финни",
  "description": "9 августа 2013 года Хэл Финни опубликовал свой последний пост на bitcointalk.",
  "tags": "[\"archives\", \"bitcoin\", \"bitcointalk\", \"cypherpunks\", \"finney\"]",
  "media": "[\"https://i.nostr.build/dwoR3.png\"]",
  "references": "[\"https://web.archive.org/web/20240207194838/https://bitcointalk.org/...\"]",
  "url_path": "/2013-08-09/hal-finneys-last-post/",
  "created_at": null,
  "updated_at": null
}
```

## Available Tags for Querying

The following tags can be used with the `/events/tags/:tag` endpoint to find relevant events. Note that tag searching is case-insensitive.

*   `clownworld`: Content related to traditional finance, banking sector announcements and activities, mainstream media narratives, and so on.
*   `cypherpunks`: Events, discussions, quotations, and news pertaining to the cypherpunk movement, including key figures like Satoshi Nakamoto, Hal Finney, David Chaum, and other early adopters, along with significant related activities.
*   `quotes`: Direct quotations featured within the event descriptions.
*   `bitcointalk`: Events or discussions from the BitcoinTalk forum.
*   `ecash`: Content pertaining to DigiCash, e-cash, or similar digital cash systems.
*   `lightning`: Events, developments, or discussions related to the Lightning Network or Lightning payments.
*   `onchain`: Events and discussions concerning on-chain Bitcoin transactions.
*   `obituaries`: Bitcoin obituaries events.
*   `trading`: Content related to Bitcoin trading, market analysis, or price charts.
*   `first`: Milestone events marking a 'first' occurrence in the Bitcoin ecosystem.
*   `scam`: Incidents involving scams, financial losses, or theft within the Bitcoin space.
*   `hack`: Incidents involving hacks or theft within the Bitcoin space.
*   `mustread`: Important documents, articles and books deemed essential reading.
*   `econ`: Discussions and events related to economic theories, principles, or impacts concerning Bitcoin.
*   `privacy`: Content focusing on privacy aspects, technologies, or discussions within the Bitcoin context.
*   `development`: Events related to Bitcoin Core releases, protocol upgrades, and technical proposals.
*   `adoption`: Events highlighting instances of countries, governments, businesses, individuals, or organizations starting to accept or use Bitcoin.
*   `legal`: For events involving lawsuits, regulations, and government legal actions.
*   `media`: To specifically categorize mentions of Bitcoin in articles, TV shows, and other media.

## Language Support

All data-retrieving endpoints support a `lang` query parameter to specify the language of the events to be queried.

*   `lang=en` (Default): Retrieves events from the English database (`events_en.db`, `DB_PATH_EN`).
*   `lang=ru`: Retrieves events from the Russian database (`events_ru.db`, `DB_PATH_RU`).

If the `lang` parameter is omitted or an unsupported value is provided, it defaults to `en`.

The two databases are **separate files with independent `id` sequences**, and they are not
guaranteed to hold the same events. Use `url_path` to relate an event across languages.

## Error Responses

Standard HTTP status codes are used. Common error responses include:

*   `400 Bad Request`: The request was malformed (e.g., missing required parameters, invalid parameter format).
*   `401 Unauthorized`: The API key is missing or invalid.
*   `404 Not Found`: The requested resource (e.g., a specific event) could not be found.
*   `429 Too Many Requests`: Rate limit exceeded.
*   `500 Internal ServerError`: An unexpected error occurred on the server.

Error responses will typically be in JSON format, like:

```json
{
  "error": "Descriptive error message"
}
```

## Endpoints

### 1. Get All Events (Paginated)

*   **Endpoint:** `/events`
*   **Method:** `GET`
*   **Description:** Retrieves a paginated list of all historical Bitcoin events, sorted by date in descending order by default. Supports language selection and flexible date filtering by year, month, and/or day. Filters can be combined (e.g., year and month, or year, month, and day).
*   **Query Parameters:**
    *   `page` (optional, integer): The page number to retrieve. Defaults to `1`.
    *   `limit` (optional, integer): The number of events per page. Defaults to `20`.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
    *   `year` (optional, string, format: `YYYY` e.g., "2022"): Year for filtering events.
    *   `month` (optional, string, format: `MM` or `M` e.g., "05" or "5"): Month for filtering events.
    *   `day` (optional, string, format: `DD` or `D` e.g., "27" or "7"): Day for filtering events.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body:**
        ```json
        {
          "events": [
            {
              "id": 1,
              "date": "2008-11-01",
              "title": "📜 Bitcoin Whitepaper Published",
              "description": "Satoshi Nakamoto publishes the Bitcoin whitepaper...",
              "tags": "[\"bitcoin\",\"whitepaper\",\"satoshi\"]",
              "media": null,
              "references": "[\"https://bitcoin.org/bitcoin.pdf\"]",
              "url_path": "/2008-11-01/bitcoin-whitepaper-published/",
              "created_at": null,
              "updated_at": null
            }
            // ... more events
          ],
          "pagination": {
            "current_page": 1, // Note: field names changed
            "per_page": 20,    // Note: field names changed
            "total": 230,
            "last_page": 12    // Note: field names changed
          }
        }
        ```
*   **Example:**
    ```bash
    # Get first 5 English events
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?page=1&limit=5&lang=en"

    # Get Russian events for December 2023
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?year=2023&month=12&lang=ru"

    # Get English events for the 15th of any month/year
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?day=15&lang=en"

    # Get English events for any day in May of any year
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?month=05&lang=en"
    
    # Get English events for the year 2021
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?year=2021&lang=en"

    # Get Russian events for May 27th, 2020
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?year=2020&month=05&day=27&lang=ru"
    ```

### 2. Search Events (FTS5)

*   **Endpoint:** `/search`
*   **Method:** `GET`
*   **Description:** Performs a full-text search across the `title`, `description`, and `tags` fields of events using SQLite's FTS5 extension. Results are sorted by relevance. Supports language selection and pagination.
*   **Query Parameters:**
    *   `q` (required, string): The search query. The query can use FTS5's syntax (e.g., `bitcoin AND halving`, `"satoshi nakamoto"`).

    **Tokenizer caveat.** Both languages use FTS5's default `unicode61` tokenizer. It
    case-folds Cyrillic correctly but does **not** stem Russian, so `биткоина` and `биткоин`
    are different tokens and return different result sets (110 vs 246 hits, overlapping in
    only 45 rows). For Russian queries, prefer a prefix match — `биткоин*` returns 361.

    *   `page` (optional, integer): The page number to retrieve. Defaults to `1`.
    *   `limit` (optional, integer): The number of events per page. Defaults to `20`.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body (follows the same structure as Get All Events `/events`):**
        ```json
        {
          "events": [
            // ... array of event objects matching the search query
          ],
          "pagination": {
            "current_page": 1,
            "per_page": 20,
            "total": 15, // Total events matching the query
            "last_page": 1
          }
        }
        ```
*   **Error Responses:**
    *   `400 Bad Request`: If the `q` query parameter is missing.
        ```json
        { "error": "Search query is required" }
        ```
*   **Example:**
    ```bash
    # Search for English events containing "Satoshi"
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/search?q=Satoshi&lang=en"

    # Search for Russian events about "whitepaper", limit to 5 results
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/search?q=whitepaper&limit=5&lang=ru"
    ```

### 3. Get Single Event by ID

*   **Endpoint:** `/events/:id`
*   **Method:** `GET`
*   **Description:** Retrieves a single historical Bitcoin event by its unique ID. Supports language selection.
*   **Path Parameters:**
    *   `id` (required, integer): The unique identifier of the event.
*   **Query Parameters:**
    *   `lang` (optional, string): Language for the event. `en` for English (default), `ru` for Russian.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body:**
        ```json
        {
          "data": { // Note: this endpoint wraps in "data"; the list endpoints use "events".
            "id": 1,
            "date": "2008-11-01",
            "title": "📜 Bitcoin Whitepaper Published",
            "description": "Satoshi Nakamoto publishes the Bitcoin whitepaper...",
            "tags": "[\"bitcoin\",\"whitepaper\",\"satoshi\"]",
            "media": null,
            "references": "[\"https://bitcoin.org/bitcoin.pdf\"]",
            "url_path": "/2008-11-01/bitcoin-whitepaper-published/",
            "created_at": null,
            "updated_at": null
          }
        }
        ```
*   **Error Responses:**
    *   `400 Bad Request`: If `id` is not a valid integer.
        ```json
        { "error": "Invalid Event ID format" }
        ```
    *   `404 Not Found`: If an event with the given `id` does not exist.
        ```json
        { "error": "Event not found" }
        ```
*   **Example:**
    ```bash
    # Get English event with ID 1
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events/1?lang=en"

    # Get Russian event with ID 20 (ID for the May 27th event in Russian DB)
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events/20?lang=ru"
    ```

### 4. Get All Unique Tags

*   **Endpoint:** `/tags`
*   **Method:** `GET`
*   **Description:** Retrieves a list of all unique tags found across all events, along with the count of events associated with each tag. Tags are returned in alphabetical order. Supports language selection.
*   **Query Parameters:**
    *   `lang` (optional, string): Language for the tags. `en` for English (default), `ru` for Russian.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body:**
        ```json
        {
          "data": [ // Note: This endpoint's response structure was not specified as changed, keeping "data" wrapper for now.
            {
              "tag": "adoption",
              "count": 72
            },
            {
              "tag": "bitcoin",
              "count": 1
            }
            // ... more tags
          ]
        }
        ```
*   **Example:**
    ```bash
    # Get English tags
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/tags?lang=en"

    # Get Russian tags
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/tags?lang=ru"
    ```

### 5. Get Events by Tag (Paginated)

*   **Endpoint:** `/events/tags/:tag`
*   **Method:** `GET`
*   **Description:** Retrieves a paginated list of historical Bitcoin events associated with a specific tag. Events are sorted by date in descending order by default. The tag search is case-insensitive. Supports language selection.
*   **Path Parameters:**
    *   `tag` (required, string): The tag to filter events by.
*   **Query Parameters:**
    *   `page` (optional, integer): The page number to retrieve. Defaults to `1`.
    *   `limit` (optional, integer): The number of events per page. Defaults to `20`.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body (follows the same structure as Get All Events `/events`):**
        ```json
        {
          "events": [
            {
              "id": 10,
              "date": "2010-05-22",
              "title": "🍕 Bitcoin Pizza Day",
              "description": "Laszlo Hanyecz made the first purchase...",
              "tags": "[\"first\",\"adoption\",\"bitcointalk\"]",
              "media": "[\"https://example.com/pizza.webp\"]",
              "references": "[\"https://bitcointalk.org/...\"]",
              "url_path": "/2010-05-22/bitcoin-pizza-day/",
              "created_at": "2026-08-08T09:59:56Z",
              "updated_at": "2026-08-08T09:59:56Z"
            }
            // ... more events with the specified tag
          ],
          "pagination": {
            "current_page": 1,
            "per_page": 20,
            "total": 5, // Total events matching the tag
            "last_page": 1
          }
        }
        ```
*   **Error Responses:**
    *   `400 Bad Request`: If `tag` parameter is missing.
        ```json
        { "error": "Tag parameter is required" }
        ```
*   **Example:**
    ```bash
    # Get first 2 English events tagged with 'adoption'
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events/tags/adoption?limit=2&lang=en"

    # Get first 2 Russian events tagged with 'adoption'
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events/tags/adoption?limit=2&lang=ru"
    ```

### 6. Health

*   **Endpoint:** `/health` — note this is at the root, **not** under `/api`
*   **Method:** `GET`
*   **Authentication:** none
*   **Description:** Reports which database artifact each language is actually being served
    from. `path` is symlink-resolved, so it names the release directory rather than
    `current`, and `sha256` is computed **once at startup** — it therefore describes the file
    the process has open, not whatever `current` points at when you ask. Those two differing
    is the failure this endpoint exists to catch. After publishing a release, compare these
    hashes against the `SHA256SUMS` that shipped with it.
*   **Success Response (200 OK):**
    ```json
    {
      "status": "ok",
      "version": "0.1.0-abc1234",
      "databases": {
        "en": {
          "path": "/srv/bitcal/data/releases/20260809T084800Z/events_en.db",
          "sha256": "cb95ad42a181aff3f2cf0579ee3f0d647b81db38c8f3228e1d0483fd69a845f2",
          "rows": 565
        },
        "ru": {
          "path": "/srv/bitcal/data/releases/20260809T084800Z/events_ru.db",
          "sha256": "b2bf2c80054f20dd47d633144d62a5edc46e2184884756fa8719325e1b42581a",
          "rows": 582
        }
      }
    }
    ```
*   **Example:**
    ```bash
    curl "http://localhost:3000/health"
    ```

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn) 