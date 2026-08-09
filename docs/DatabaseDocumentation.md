[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)

# Bitcoin Historical Events SQLite Database Documentation

This document describes the SQLite databases the API reads.

## Where the databases come from

**They are not in this repository, and this service does not create or modify them.** Events
are authored and validated outside this repo, and published to the server as an immutable
artifact:

*   The service reads `/srv/bitcal/data/current/events_{en,ru}.db`, where `current` is a
    symlink to a timestamped release directory.
*   The files are owned by `deploy`, mode `0444`, in a directory the service user cannot
    write. **The read-only rule is enforced by a permission bit, not by convention.**
*   The API opens them with `file:<path>?mode=ro&_query_only=1`. It runs no `AutoMigrate`, no
    `CREATE TABLE`, no `CREATE INDEX` and no trigger creation. Every one of those existed in
    an earlier version and every one has been removed: the artifact ships with its own
    indexes, triggers and FTS index, and recreating them at boot is how a deployed copy
    silently diverges from canonical.
*   Paths come from `DB_PATH_EN` and `DB_PATH_RU`. There are **no defaults** — the service
    exits at startup if either is unset.

`GET /health` reports the resolved path, SHA-256 and row count of each file actually open, so
"which copy is the server serving" is a one-line question. See `docs/APIDocumentation.md`.

Two invariants hold for a publishable artifact, and both are about journalling:

1.  **The file must not be in WAL mode.** WAL needs its `-shm` wal-index, so opening one
    read-only requires either that the sidecars already exist or that the directory is
    writable. Neither holds. Check bytes 18/19 of the header: `01 01` is delete mode, `02 02`
    is WAL.
2.  **Ship the bare `.db`** — no `-wal`, `-shm` or `-journal` beside it. A *hot* rollback
    journal must be replayed before the file can be read, and replay is a write. You cannot
    tell a hot journal from a stale one by looking, so the rule is about presence.

## Table: `events`

Present in both database files, with identical definitions.

### Columns

| Column        | Declared type | Constraints   | Description |
|---------------|---------------|---------------|-------------|
| `id`          | `INTEGER`     | `PRIMARY KEY` | Unique within **one** database. RU `100` and EN `100` are unrelated events. |
| `date`        | `date`        | `NOT NULL`    | `YYYY-MM-DD` text. The range starts at **1881-09-29**, before the Unix epoch. See the note below on the declared type. |
| `title`       | `TEXT`        |               | |
| `description` | `TEXT`        |               | |
| `media`       | `TEXT`        |               | JSON array of URLs, stored as text. **`NULL` when absent — never `''`, never `'[]'`.** |
| `tags`        | `TEXT`        |               | JSON array of strings, stored as text. |
| `references`  | `TEXT`        |               | JSON array of URLs, stored as text. Same `NULL` rule as `media`. **`references` is a SQL reserved word and must be quoted** as `"references"` in every statement; unquoted, SQLite refuses to parse. |
| `created_at`  | `datetime`    |               | Row bookkeeping. `NULL` on most rows. |
| `updated_at`  | `datetime`    |               | Row bookkeeping. `NULL` on most rows. |
| `url_path`    | `TEXT`        | `UNIQUE` index | `/<date>/<slug>/`. The website's page path and the cross-language join key. |

**On `date`'s declared type.** SQLite has no date type; the value is text. But the column is
*declared* `date`, and `mattn/go-sqlite3` converts any column declared `date`, `datetime` or
`timestamp` into a `time.Time` inside the driver, before the ORM sees it — unconditionally,
with no way to switch it off. That is why `Event.Date` is a `DateString` with a custom
`Scan` in `database.go`: without it the API emits `"1881-09-29T00:00:00Z"`, inventing a time
and a timezone. The same conversion applies to `created_at`/`updated_at`, where it is correct,
because those really are timestamps.

### Indexes

*   `idx_events_date` on `date`
*   `idx_events_tags` on `tags` — of little use, since `tags` holds a JSON blob
*   `idx_events_url_path` on `url_path`, **UNIQUE**
*   the implicit index on `id` as `PRIMARY KEY`

## Full-text search: `events_fts`

Both databases carry an FTS5 index and its three triggers. **Both are fully populated**
(RU 582/582, EN 565/565).

```sql
CREATE VIRTUAL TABLE events_fts USING fts5(
    title, description, tags,
    content='events', content_rowid='id'
);
```

*   **External content.** The index stores no copy of the text; it reads through to `events`.
    A consequence worth knowing: `SELECT count(*) FROM events_fts` reads through to the
    content table and reports a healthy row count even over an index holding nothing. To
    check whether the index is real, compare against `events_fts_docsize`:

    ```sql
    SELECT (SELECT count(*) FROM events) = (SELECT count(*) FROM events_fts_docsize);
    ```

*   **Triggers.** `events_ai`, `events_ad` and `events_au` keep the index in step with writes
    to `events`. **They ship inside the artifact and are never created by this service.** An
    earlier version of `InitDB` created a second, differently named set
    (`events_after_insert`/`_delete`/`_update`); had both sets coexisted, every write would
    have fired twice.
*   **Build requirement.** FTS5 is not compiled into `mattn/go-sqlite3` by default. The binary
    must be built with `-tags fts5` (see the `Makefile`) or every query touching `events_fts`
    fails at runtime with `no such module: fts5`.
*   **Tokenizer.** Default `unicode61` in both languages. It case-folds Cyrillic but does not
    stem Russian.

## The two databases are not symmetric

`events`, `events_fts` and the FTS shadow tables exist in both. **English additionally has**
`references_details`, `event_references_link` (which carries foreign keys onto `events(id)`),
and `references_details_fts` with its own shadow tables. Russian has none of these. Do not
assume a table exists in both files because you found it in one.

## Notes on `tags`, `media` and `references` storage

*   **JSON arrays as strings.** These three columns hold JSON encoded as text, and the API
    passes the string through unparsed. Clients must parse them.
*   **Absence is `NULL`,** and only `NULL`. All three representations (`NULL`, `''`, `'[]'`)
    were in use across the two databases until they were normalised; `NULL` won because
    absence here is genuinely absence rather than a list someone verified was empty. The
    practical consequence for clients: you cannot assume the value parses as an array, so
    branch on null first.
*   **Tag counting** uses `json_each` over the column (`/api/tags`). Note that
    `/api/events/tags/:tag` still uses a `LIKE` substring match against the JSON text, which
    is not the same thing — a known inconsistency, not yet fixed.
*   **Media URLs are live and self-hosted**, and the API's job is to pass the strings through.
    It does not proxy, validate or rewrite them.

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)
