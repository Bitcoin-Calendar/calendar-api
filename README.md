[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)

# Bitcoin Calendar API

A small, **read-only** Go service that serves the Bitcoin Calendar event databases over HTTP,
in English and Russian.

It is read-only in a structural sense, not as a convention. The databases are an artifact
authored and validated in a separate repository and shipped here whole; this service opens
them with `mode=ro`, never migrates them, and never creates the indexes, triggers or
full-text tables they already carry. On the server the files are mode `0444` in a `0555`
directory, and systemd's `ReadOnlyPaths` makes the kernel refuse a write even if a future
code change tried one. That arrangement exists because the project previously had eighteen
divergent copies of the same database and no way to tell which one any given consumer was
reading.

## Quick start

```sh
make build          # CGO_ENABLED=1 go build -tags fts5 …
make test           # black-box: builds the binary, serves a fixture, drives it over HTTP

DB_PATH_EN=/path/events_en.db \
DB_PATH_RU=/path/events_ru.db \
API_KEYS=some-secret \
./bitcal-api

curl localhost:3000/health | jq .
curl -H "X-API-KEY: some-secret" 'localhost:3000/api/events?month=8&day=9&lang=ru' | jq .
```

Two things about the build are not optional:

1.  **`-tags fts5`.** The SQLite driver does not compile FTS5 in by default, and without it
    every query touching `events_fts` fails at runtime while the rest of the API looks fine.
    A build without the tag is refused outright — see `fts5_required.go`.
2.  **Build on Ubuntu/glibc**, or in a matching container (`make build-ubuntu`). CGO ties the
    binary to the C library it was built against; one built on macOS or Alpine will not start
    on the server at all.

## Endpoints

Everything under `/api` requires an `X-API-KEY` header. `/health` and `/public/v1/events` do not.

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | Which artifact this process has open, and whether it is fully indexed. Unauthenticated. |
| `GET /public/v1/events` | Every event for one language as a single document, for the website. Unauthenticated, unpaginated, with a strong `ETag`. |
| `GET /api/events` | Events, paginated. Filter with `year`, `month`, `day`, `category`, `landmark`. |
| `GET /api/events/:id` | One event. |
| `GET /api/events/tags/:tag` | Events carrying a tag. |
| `GET /api/tags` | Every tag with the number of events carrying it. |
| `GET /api/categories` | Every category with the number of events carrying it. |
| `GET /api/search?q=` | Full-text search over title, description and tags. |

All of them take `lang=en` (default) or `lang=ru`. Full detail, including every field and
every error, is in [docs/APIDocumentation.md](docs/APIDocumentation.md).

### The public document

`/public/v1/events` is the one read that needs no key, because its consumer is a public
client the service cannot hand a secret to: the static website (`gm-web`) fetches it once
from Node at build time, and anyone else may read it the same way. It is not a browser
fetch, so no CORS origin needs to be added for it. It differs from `/api/events` on purpose:

*   **The whole artifact in one body**, in the same order as `/api/events` (date DESC, id DESC).
    No pagination.
*   **A smaller event**: `id`, `date`, `title`, `description`, `references`, `media`.
*   **`references` and `media` are real arrays of strings**, `[]` when absent — never `null`,
    never a JSON-encoded string. A malformed stored value is normalised to the strings it
    holds rather than dropping the event, and the top-level `invalid_reference_fields` /
    `invalid_media_fields` counters say how many fields that happened to. Strings are
    preserved as stored; deciding what is a URL is the consumer's job.
*   **`schema` is `bitcoin-calendar.public-events.v1`**, and `database.sha256` / `database.rows`
    are the same values `/health` reports for that language.
*   **Strong `ETag`** per service build, language and artifact; `If-None-Match` answers `304`.
    It changes when a new artifact is published or a new build of this service is deployed,
    since either can change the bytes.

`lang` behaves as everywhere else — `en` by default, unknown values fall back to `en` — and
the `language` field names the artifact actually served. This route and `/health` are
rate-limited by connecting IP, whatever `X-API-KEY` a caller sends: only under `/api` does
the key choose the bucket, because only there is it checked. Direct callers using the same
loopback address share one bucket; nginx-proxied callers **all** collapse into nginx's loopback bucket, because the app
sees nginx's address for every one of them — and that bucket overlaps the operator's direct
`/health` calls whenever they arrive from the same address. A public rollout therefore puts a cache in front of
this route (revalidating on the `ETag`) and owns the external per-client and aggregate
limits; the app limiter is not that protection. Exceeding the app limit is a `429`, and a
genuine server fault a `500`, as everywhere else.

## Things that will bite a client

These are the parts that are not guessable from the endpoint list. Each was a real bug.

*   **`date` is a string, `"2013-08-09"`** — not a timestamp. The range starts at 1881-09-29,
    before the Unix epoch, and there is no time-of-day component to report. The column's
    *declared* type is `date`, which makes the SQLite driver convert it to a `time.Time`
    before GORM sees it, so `database.go` carries a scanner that converts it back. Without
    that the API emits `"1881-09-29T00:00:00Z"` — an invented time and timezone.
*   **`media` and `references` are JSON arrays encoded as strings**, and `null` when absent —
    never `""` and never `"[]"`. Decode them a second time. `null` means "no media", which is
    not the same claim as "an empty list of media".
*   **`created_at` and `updated_at` are frequently `null`.** Many rows genuinely have no
    timestamp. Rendering those as `"0001-01-01T00:00:00Z"` would be inventing data.
*   **`url_path`** (`/2013-08-09/hal-finneys-last-post/`) is the cross-language join key and
    the website's page URL. It is present on every row.
*   **`category` is not `tags[0]`.** Every event has exactly one `category`, and it is what the
    website colours and filters by. Tag order carries no meaning, so deriving it from the
    first tag is wrong. The tag and category vocabularies do not correspond at all: `first` is
    a tag on ~104 rows and a category on none, and `bitcoin` is neither — so
    `/api/events/tags/bitcoin` returns an empty list and `?category=bitcoin` is a `400`.
    Filter with `/api/events?category=…` and discover the values with `/api/categories`. The
    set is closed but **owned by the data and liable to change in both directions** — it was
    rewritten wholesale on 2026-08-12 — so accept unrecognised values rather than hardcoding
    the list. The service derives the accepted values from the artifact at startup for that
    reason; an unknown category is a `400`, not an empty list.
*   **`landmark` is a boolean, orthogonal to `category`.** It marks the events that matter to a
    bitcoiner — 402 of 581 RU and 394 of 565 EN — and drives one UI control, the "Только
    главное" switch. Filter with `/api/events?landmark=true`, which ANDs with `?category=` and
    the date filters. Always `true` or `false`, never `null`. Added 2026-08-12: an artifact
    published before then has no such column, and the service then reports `false` everywhere
    and rejects `?landmark=` with a `400` rather than answering a misleading empty list.
    It is a filter on `/api/events` **only** — sending it to `/api/search` or
    `/api/events/tags/:tag` is a `400` too, rather than a `200` full of unfiltered results.
*   **`events` is always an array**, `[]` when nothing matches, on every endpoint that
    returns a list — and so is `data` on `/api/tags` and `/api/categories`. Never `null`.
*   **An unknown `lang` silently serves English.** `lang=xx` is not an error. Do not rely on
    a typo being caught.
*   **`/api/tags` returns its list under `data`**, not `tags`.
*   **A malformed `month`, `day` or `year` is a 400**, deliberately. An empty list is
    indistinguishable from a day that has no events, so a client with a bug in its date
    arithmetic would post nothing and report success forever.
*   **A malformed search query is a 400**, not a 500. Bare `AND`/`OR`/`NOT`, unbalanced
    parentheses or quotes, and a leading `*` are all invalid FTS5. Prefix search
    (`биткоин*`) and `OR`/`NEAR` do work.
*   **Quoting `q` is a phrase search.** `q="bitcoin price"` wants those words adjacent, in
    that order — 6 hits against the English artifact. Unquoted, `q=bitcoin price` is an
    implicit `AND` and finds 39.
*   **`limit` is capped at 1000 and `page` at 1000000**, and both are a 400 when out of range
    or unparseable rather than being clamped. The ceiling sits above the corpus on purpose —
    `limit=1000` returns every event for a language — so it refuses only values that cannot
    be meant seriously.
*   **Lists are newest first, ties broken by `id` descending.** 19 dates carry more than one
    event in English, and without the tiebreaker paging across one could show an event twice
    and skip another.
*   **Rate limiting under `/api` is 100/min per API key.** Give each consumer its own key: they all reach
    the service over loopback, so anything keyed per-IP would be one shared budget.

## Health

```json
{
  "status": "ok",
  "version": "0.1.0-82cafb9",
  "databases": {
    "ru": {
      "path": "/srv/bitcal/data/releases/20260813T054356Z/events_ru.db",
      "sha256": "5bdc8c3e…",
      "rows": 581,
      "fts": { "indexed": 581, "consistent": true },
      "categories": { "present": true, "count": 8 },
      "landmark": { "present": true, "count": 402 }
    }
  }
}
```

`path` is symlink-resolved and `sha256` is computed once at startup, so this describes the
inode the process actually has open rather than whatever `current` points at when you ask.
Those two differing is the failure the endpoint exists to catch.

`fts.consistent` is `indexed == rows`: every event is reachable by search. When it is false,
`status` becomes `degraded` — the service is up and search is silently incomplete. `status`
reports full-text coverage only; the two blocks below never affect it, because serving an
artifact that predates a column is what a rollback looks like.

`categories` is what the service read out of the artifact at startup, and it is what
`?category=` validates against. `count: 0` means every category filter is rejected — either
the artifact predates the column (`present: false`) or no row carries a value (`present:
true`, which should never have been published; `publish-db.sh` refuses to leave a release in
that state).

`landmark` reports the same two things for the flag, and `count` is exactly what
`?landmark=true` returns. It differs in what `count: 0` means: no row carrying the flag is
legal data — the validator sets no target fraction for an editorial judgement — so
`?landmark=true` answers an empty list and `publish-db.sh` warns rather than refusing.

**The service refuses to start** if a full-text index is missing, empty or unreadable. That
is deliberate: a broken index makes `/api/search` return an empty result set, which is
indistinguishable from a query that matched nothing, so nothing downstream could ever report
it. Startup is the only place it can be made loud.

## Deployment

A native systemd service, not Docker. The unit is `deploy/bitcal-api.service`; it binds
loopback only and is not proxied by nginx.

There are two release paths and they do not overlap:

*   **`deploy/publish-db.sh`** ships database artifacts. It validates the source, stages it,
    validates the staged copy again, checks that this API can actually emit the staged
    schema, flips the `current` symlink, restarts the service — mandatory, since SQLite holds
    an open file descriptor and the symlink alone changes nothing — then verifies `/health`.
*   **`deploy/publish-api.sh`** ships the binary. It builds on the box (CGO ties the binary to
    the target's glibc), runs the suite there, backs the running binary aside, installs,
    restarts, and asserts `/health` shows a new version **and unchanged data**.

Both roll back automatically if anything after the irreversible step fails. Because they are
independent, the binary and the data drift; check both halves of `/health` when diagnosing.

See [docs/Deployment.md](docs/Deployment.md).

## Tests

The suite lives in `tests/` and is black-box: it builds this binary, stages a database fixture
exactly as a release is staged (`0444` in a `0555` directory), starts the service against it
and drives it over HTTP. So `make test` also exercises the build tag, the read-only open, the
boot probe and the JSON contract above — none of which a unit test calling `InitDB` directly
would prove.

## Documentation

*   [docs/APIDocumentation.md](docs/APIDocumentation.md) — every endpoint, field and error
*   [docs/DatabaseDocumentation.md](docs/DatabaseDocumentation.md) — schema and the artifact contract
*   [docs/Deployment.md](docs/Deployment.md) — building, installing, releasing, troubleshooting

## Environment

| Variable | |
| --- | --- |
| `API_KEYS` | **Required.** Comma-separated. The service exits if unset. |
| `DB_PATH_EN` | **Required.** No default — the service exits naming the variable. |
| `DB_PATH_RU` | **Required.** Same. |
| `LISTEN_ADDR` | Defaults to `127.0.0.1:3000`. |
| `CORS_ALLOWED_ORIGINS` | Comma-separated. Defaults to `http://localhost:3000`. |

## Access

The API will be publicly available in Q3 2026. To test it before then, DM
[@Tony](https://njump.me/npub10awzknjg5r5lajnr53438ndcyjylgqsrnrtq5grs495v42qc6awsj45ys7)
on Nostr and I'll share a key.
