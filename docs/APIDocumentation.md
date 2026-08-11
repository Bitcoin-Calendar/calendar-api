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

**100 requests per minute, per API key.** Each caller should therefore have its own key —
ask for one rather than sharing.

It is keyed on the API key rather than the client address deliberately: every consumer of
this service runs on the same host and reaches it over loopback, so a per-IP limit would put
all of them in one shared bucket, where they would throttle each other with intermittent
`429`s as the only symptom. Requests with no API key (`/health`, `/metrics`) fall back to
per-IP.

The response carries `X-RateLimit-Limit`, `X-RateLimit-Remaining` and `X-RateLimit-Reset`.

## Timeouts

Every endpoint that reads the database carries a **5 second deadline**, enforced on the query
itself rather than merely on the caller. A request that exceeds it is answered `408 Request
Timeout`. Nothing legitimate comes close — these queries run in microseconds over a few
hundred rows — so a 408 means something is wrong, not that you asked for too much.

The connection is bounded separately: 10s to send a request, 15s to receive a response, 60s
idle.

## Overview of Endpoints

The API provides the following main functionalities:

*   **`/events`**: Retrieve a paginated list of all events, with powerful filtering by date (year, month, day, or combinations), category, and language.
*   **`/events/:id`**: Fetch a single event by its unique ID.
*   **`/search`**: Perform a full-text search across event titles, descriptions, and tags.
*   **`/tags`**: Get a list of all unique event tags and their usage counts.
*   **`/categories`**: Get every category and its event count — what a client needs to build a category filter.
*   **`/events/tags/:tag`**: Retrieve a paginated list of events associated with a specific tag.
*   **`/health`**: Report which database artifact the service has open. **Not** under `/api`, and needs no API key.

Detailed information for each endpoint is provided below.

## The event object

Every endpoint that returns events returns objects of this shape — including `/search`, which
until 2026-08-10 silently omitted `category`, `created_at` and `updated_at` because it
enumerates its columns by hand. Several fields below are worth reading before writing a
client, because they have changed:

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
| `category` | string | The event's single classification. Always present, never empty, always one of a closed set — but **the set is owned by the data, not by this API, and it grows**: `security` was added on 2026-08-10, a day after the column itself. Treat an unrecognised value as valid and render it; do not hardcode the list into a client. As of 2026-08-10 it is: `archives`, `bitcoin`, `first`, `holiday`, `legal`, `lightning`, `macro`, `mining`, `mustread`, `obituary`, `price`, `privacy`, `scam`, `security`, `software`. **Do not derive it from `tags[0]`** — that inference used to work and is now wrong, because tag order carries no meaning. In particular `bitcoin` is a category with no corresponding tag left in the data at all, so `/api/events/tags/bitcoin` returns nothing while 132 RU and 66 EN events are `category: "bitcoin"`. |
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
  "category": "archives",
  "created_at": null,
  "updated_at": null
}
```

## Available Tags for Querying

**`GET /api/tags` is the authoritative list.** It returns every tag in the requested language
with its usage count, straight from the artifact, so it is correct by construction. Prefer it
to anything written here: as of 2026-08-10 there are **192 distinct tags in each language**,
and the selection below is an orientation aid, not a catalogue.

Tag matching on `/events/tags/:tag` is case-insensitive.

> **If you hardcoded tags from an earlier version of this document, re-check them.** Three
> entries listed here until 2026-08-10 match **nothing** in the data, and a tag with no events
> is not an error — it is an empty list, indistinguishable from a tag that simply has no events
> yet. Two of the three have live near-misses that make the mistake look like your typo:
>
> | Was documented | Status | Did you mean |
> |---|---|---|
> | `obituaries` | never existed | `obituary` |
> | `econ` | never existed | `economics` |
> | `clownworld` | retired from the data | — |
>
> Separately, **`bitcoin` is not a tag.** It was retired from every row and now exists only as
> a `category`, so `/api/events/tags/bitcoin` returns an empty list. See the `category` field
> above.

The most-used tags, by event count as of 2026-08-10 (English; Russian is within a row or two):

*   `cypherpunks` (118): the cypherpunk movement and its figures — Satoshi Nakamoto, Hal Finney, David Chaum and other early adopters.
*   `first` (103): milestone events marking a first occurrence in the Bitcoin ecosystem.
*   `archives` (97): material preserved from primary sources.
*   `mustread` (66): documents, articles and books deemed essential reading.
*   `legal` (62): lawsuits, regulations and government legal action.
*   `privacy` (60): privacy aspects, technologies and discussions.
*   `satoshi` (60): events concerning Satoshi Nakamoto specifically.
*   `prebitcoin` (57): events before Bitcoin itself, including its intellectual ancestry.
*   `security` (52): vulnerabilities, disclosures and defensive practice.
*   `macro` (49): macroeconomic events and their bearing on Bitcoin.
*   `obituary` (49): the Bitcoin obituaries.
*   `software` (46): releases and client software.
*   `scam` (40): scams, financial losses and theft.
*   `development` (39): Bitcoin Core releases, protocol upgrades and technical proposals.
*   `bitcointalk` (37): events and discussions from the BitcoinTalk forum.
*   `mining` (36): mining, hardware and difficulty.
*   `cryptography` (35): cryptographic primitives and research.
*   `media` (32): mentions of Bitcoin in articles, TV and other media.
*   `price` (27): price action and market milestones.
*   `finney` (26): Hal Finney specifically.

Also present and self-explanatory: `onchain`, `lightning`, `hack`, `ecash`, `holiday`,
`trading`, `quotes`, `mtgox`, `silkroad`, `wikileaks`, `freedom`, `protocol`, `bip`,
`halving`, `hashrate`, `economics`, `shitcoin`, `meme`, `szabo`, `salvador`, `adoption`, and
roughly 170 more. Call `/api/tags` for the full set.

## Language Support

All data-retrieving endpoints support a `lang` query parameter to specify the language of the events to be queried.

*   `lang=en` (Default): Retrieves events from the English database (`events_en.db`, `DB_PATH_EN`).
*   `lang=ru`: Retrieves events from the Russian database (`events_ru.db`, `DB_PATH_RU`).

If the `lang` parameter is omitted or an unsupported value is provided, it defaults to `en`.

The two databases are **separate files with independent `id` sequences**, and they are not
guaranteed to hold the same events. Use `url_path` to relate an event across languages.

## Empty results

Every endpoint that returns a list returns `"events": []` when nothing matches — never
`null`. Search was the one exception until it was fixed, so a client written against the old
behaviour may still carry a needless null check.

An unknown `lang` is **not** an error: it falls back to English. Do not rely on a typo in
`lang` being caught.

## Error Responses

Standard HTTP status codes are used. Common error responses include:

*   `400 Bad Request`: The request was malformed. This covers a missing `q`, a `month`,
    `day` or `year` that is not a number in range, and a search string that is not a valid
    FTS5 expression. See *Rejected input* below.
*   `401 Unauthorized`: The API key is missing or invalid.
*   `404 Not Found`: The requested resource (e.g., a specific event) could not be found.
*   `405 Method Not Allowed`: The service is read-only; only `GET`, `HEAD` and `OPTIONS`.
*   `408 Request Timeout`: The query exceeded its 5 second deadline. See Timeouts above.
*   `429 Too Many Requests`: Rate limit exceeded.
*   `500 Internal Server Error`: An unexpected error occurred on the server. A `5xx` here
    means a genuine server fault — bad input is never reported this way.

### Rejected input

Four classes of input are answered `400` rather than being quietly absorbed, because in each
case the alternative response is indistinguishable from a legitimate result:

**Date filters.** `month`, `day` and `year` must be plain integers in range (`1`–`12`,
`1`–`31`, four digits). Both `8` and `08` are accepted. Anything else — `abc`, `13`, `0`,
`8.0`, `+8`, `" 8"` — is rejected. Previously these matched nothing and returned `200` with
an empty list, which reads exactly like a day with no events: a client with a bug in its
date arithmetic would report success indefinitely.

**Pagination.** `page` must be an integer in `1`–`1000000`, and `limit` an integer in
`1`–`1000`. Anything else — `abc`, `0`, `-5`, `1001`, `100000` — is rejected. These used to
be silently replaced with page 1 of 20, so a caller asking for 500 events got 20 and a `200`,
with nothing to say it had been overruled.

The `limit` ceiling is a backstop against values that cannot be meant seriously, not a
page-size discipline: 1000 is above the corpus, so `limit=1000` legitimately returns every
event for a language in one response. It exists because without any bound `limit=100000` is
accepted as readily as `limit=20`, and nothing in the response then says the endpoint is
paginated at all.

**Category.** `category` must be one the artifact actually carries. An unknown value is
rejected with a message naming the accepted set, rather than returning `200` with an empty
list — which would be indistinguishable from a category that genuinely has no events, so a
client could not tell its own typo from a quiet corner of the corpus. Matching is
case-insensitive.

The accepted set is **read from the artifact when the service starts**, not compiled into the
binary. That is deliberate: canonical owns this vocabulary and it grows — `security` was added
on 2026-08-10 — and a hardcoded list would reject a valid new category until a new binary was
built *and* deployed, turning a content edit into a code release. Call `/api/categories` for
the current set.

If the artifact being served has no categories at all — it predates the column, or no row
carries a value — then **every** `category` value is rejected and `/api/categories` is empty.
The message says which of the two it is. Nothing can match in either case, so the alternative
would again be a `200` and an empty list.

`category` is a filter on `/events` only. Sending it to `/search` or `/events/tags/:tag` is a
`400` rather than being quietly dropped: those endpoints do not narrow by category, and a
`200` full of unfiltered results is indistinguishable from a filter that ran. (`/events/:id`
ignores it, like any other stray parameter: a single-event fetch is not a list a filter could
narrow, and the response shows the event's real `category`.)

**Search expressions.** `q` is passed to SQLite's FTS5 parser, which rejects bare operators
(`AND`, `OR`, `NOT`), unbalanced parentheses or quotes, and a leading `*`. These are things a
person can reasonably type into a search box, so they are client errors, not server errors.
Prefix search (`биткоин*`), `OR` and `NEAR` all work normally.

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
*   **Description:** Retrieves a paginated list of all historical Bitcoin events, newest first — `date` descending, with `id` descending breaking ties between events on the same day. Supports language selection and flexible date filtering by year, month, and/or day. Filters can be combined (e.g., year and month, or year, month, and day).
*   **Query Parameters:**
    *   `page` (optional, integer): The page number to retrieve. `1`–`1000000`, defaults to `1`. Out of range or unparseable is a `400`.
    *   `limit` (optional, integer): The number of events per page. `1`–`1000`, defaults to `20`. Out of range or unparseable is a `400` — it is not clamped.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
    *   `year` (optional, string, format: `YYYY` e.g., "2022"): Year for filtering events.
    *   `month` (optional, string, format: `MM` or `M` e.g., "05" or "5"): Month for filtering events.
    *   `day` (optional, string, format: `DD` or `D` e.g., "27" or "7"): Day for filtering events.
    *   `category` (optional, string): Return only events with this category. Case-insensitive. **An unrecognised value is a `400`**, not an empty list — see *Rejected input*. Combines with the date filters (they AND together), so `?category=bitcoin&month=5` is "bitcoin events in May". Call `/api/categories` for the valid values and their counts; the service derives them from the artifact at startup, so a category canonical adds is accepted as soon as its data is published.
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
              "category": "mustread",
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
*   **Description:** Performs a full-text search across the `title`, `description`, and `tags` fields of events using SQLite's FTS5 extension. Results are sorted by relevance, with `id` descending breaking ties between equally-ranked rows so that paging cannot repeat one event and drop another. Supports language selection and pagination.
*   **Query Parameters:**
    *   `q` (required, string): The search query. The query can use FTS5's syntax (e.g., `bitcoin AND halving`, `"satoshi nakamoto"`).

    **Quoting is a phrase search.** `q="bitcoin price"` matches only rows carrying those two
    words adjacent and in that order; unquoted, `q=bitcoin price` is an implicit `AND` and
    matches rows carrying both anywhere. Against the English artifact that is 6 hits versus
    39. Until 2026-08-09 the handler doubled every `"` before passing the string to FTS5,
    which turned a phrase into an implicit `AND` — so quoting did nothing, word order was
    ignored, and a stray `"` answered `200` instead of `400`. A client written against that
    behaviour may be relying on quotes being harmless.

    **Tokenizer caveat.** Both languages use FTS5's default `unicode61` tokenizer. It
    case-folds Cyrillic correctly but does **not** stem Russian, so `биткоина` and `биткоин`
    are different tokens and return different result sets (110 vs 246 hits, overlapping in
    only 45 rows). For Russian queries, prefer a prefix match — `биткоин*` returns 361.

    *   `page` (optional, integer): The page number to retrieve. `1`–`1000000`, defaults to `1`. Out of range or unparseable is a `400`.
    *   `limit` (optional, integer): The number of events per page. `1`–`1000`, defaults to `20`. Out of range or unparseable is a `400` — it is not clamped.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
    *   `category`: **not supported here** — sending it is a `400`, not a silently unfiltered result. Search does not narrow by category; put the term in `q`, or filter on `/events`.
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
            "category": "mustread",
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
          "data": [
            {
              "tag": "adoption",
              "count": 3
            },
            {
              "tag": "archives",
              "count": 97
            }
            // ... 190 more tags
          ]
        }
        ```

    Counts are **events, not occurrences**: a row listing the same tag twice counts once, and
    this number always equals `pagination.total` from `/events/tags/:tag`.

*   **Example:**
    ```bash
    # Get English tags
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/tags?lang=en"

    # Get Russian tags
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/tags?lang=ru"
    ```

### 5. Get All Categories

*   **Endpoint:** `/categories`
*   **Method:** `GET`
*   **Description:** Every category present in the language's artifact, with the number of events carrying it, alphabetically. This is what a client needs to build a category filter without fetching every event to discover what exists — and it is the authoritative list, read from the data rather than from any document.
*   **Query Parameters:**
    *   `lang` (optional, string): Language for the categories. `en` for English (default), `ru` for Russian.
*   **Request Body:** None
*   **Success Response (200 OK):**
    *   **Content-Type:** `application/json`
    *   **Body:**
        ```json
        {
          "data": [
            {
              "category": "archives",
              "count": 89
            },
            {
              "category": "bitcoin",
              "count": 66
            }
            // ... more categories
          ]
        }
        ```

    Unlike `tags`, every event has **exactly one** category, so these counts sum to the total
    number of events and always equal `pagination.total` from `/events?category=`.

    The two languages carry the same set of category *names* but different counts, and they
    disagree about the category of 62 of the 562 events they share by `url_path`. That is
    catalogued upstream and is data, not a bug — a two-language filter will return genuinely
    different sets for those events.

*   **Example:**
    ```bash
    # Get English categories
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/categories?lang=en"

    # Then filter by one
    curl -H "X-API-KEY: your_api_key" "http://localhost:3000/api/events?lang=en&category=bitcoin&limit=5"
    ```

### 6. Get Events by Tag (Paginated)

*   **Endpoint:** `/events/tags/:tag`
*   **Method:** `GET`
*   **Description:** Retrieves a paginated list of historical Bitcoin events associated with a specific tag, newest first — `date` descending, with `id` descending breaking ties between events on the same day. The tag search is case-insensitive. Supports language selection.
*   **Path Parameters:**
    *   `tag` (required, string): The tag to filter events by.
*   **Query Parameters:**
    *   `page` (optional, integer): The page number to retrieve. `1`–`1000000`, defaults to `1`. Out of range or unparseable is a `400`.
    *   `limit` (optional, integer): The number of events per page. `1`–`1000`, defaults to `20`. Out of range or unparseable is a `400` — it is not clamped.
    *   `lang` (optional, string): Language for the events. `en` for English (default), `ru` for Russian.
    *   `category`: **not supported here** — sending it is a `400`. This endpoint filters by tag only. `category` and `tags` are different fields, so there is no equivalent to narrowing one by the other; filter by category on `/events` instead.
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
              "category": "bitcoin",
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

### 7. Health

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
          "path": "/srv/bitcal/data/releases/20260810T131954Z/events_en.db",
          "sha256": "6abda1c576b81220538b35d2d697064ac5a0ea72ecc6772d9c58832cdcf8f80e",
          "rows": 565,
          "fts": { "indexed": 565, "consistent": true },
          "categories": { "present": true, "count": 15 }
        },
        "ru": {
          "path": "/srv/bitcal/data/releases/20260810T131954Z/events_ru.db",
          "sha256": "12a5f04093e31ceb0a34cf44b60f4d5758869c96e990bb5b840ac3f983b45ba4",
          "rows": 581,
          "fts": { "indexed": 581, "consistent": true },
          "categories": { "present": true, "count": 15 }
        }
      }
    }
    ```

#### Fields

| Field | Meaning |
| --- | --- |
| `status` | `ok`, or `degraded` when some artifact's full-text index does not cover every row. |
| `version` | Baked in at build time. `dev` means it was built without the Makefile. |
| `path` | Symlink-resolved path of the file this process has open. |
| `sha256` | Hash of that file, computed once at startup. |
| `rows` | Rows in `events`. |
| `fts.indexed` | Documents in the full-text index, read from `events_fts_docsize`. |
| `fts.consistent` | `fts.indexed == rows` — every event is reachable by `/api/search`. |
| `categories.present` | Whether the artifact has a `category` column at all. `false` means it predates 2026-08-09. |
| `categories.count` | Distinct categories the service read from it at startup — the exact set `?category=` validates against, and the same list `/api/categories` returns. |

`categories` is the one part of this document that describes the artifact's *contents* rather
than its shape, and it is here for the release check: when `count` is `0`, every `?category=`
is rejected and `/api/categories` is empty, and nothing else outside the boot log says so.
The two fields are separate because they mean different things — `present: false` is an
artifact older than the column, which is a legitimate rollback target; `present: true` with
`count: 0` is a column no row carries a value in, which should never have been published.

#### On the status code

`/health` answers **200 whenever the service is serving**, including when `status` is
`degraded`. A degraded process is answering every endpoint correctly except for the
completeness of search, and returning a failure code would have a load balancer pull it for
no good reason. **Read the `status` field, not the HTTP code.**

`status` reports full-text coverage only. An absent or empty category vocabulary does **not**
make it `degraded`: serving an artifact that predates the column is what a rollback looks
like, and a dashboard alarming for the duration of one is pressure to end the rollback rather
than to fix the release. Read `categories` for that.

The states that would make search useless rather than incomplete — the index missing, empty,
or unreadable — are not reported here at all, because the service refuses to start on them.
See *Startup checks* in [Deployment](Deployment.md).

*   **Example:**
    ```bash
    curl "http://localhost:3000/health"

    # In a release check — asserts the service is serving what you published,
    # and that all of it is searchable.
    curl -sf localhost:3000/health | jq -e '
      .status == "ok" and (.databases | to_entries | all(.value.fts.consistent))'
    ```

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn) 