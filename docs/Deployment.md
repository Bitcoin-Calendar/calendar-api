[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)

# Deployment Guide

The API runs as a **native systemd service**, not in Docker. The `Dockerfile` and
`docker-compose.yml` this guide used to describe have been removed: nothing deployed with
them, they built against musl for a glibc host, and the `Dockerfile` was the only place
recording that the binary needs `-tags fts5`. That knowledge now lives in the `Makefile`.

## Target

|  |  |
|---|---|
| host | Ubuntu 24.04 |
| runs as | user `bitcal`, systemd unit `bitcal-api.service` |
| binds | `127.0.0.1:3000` only — no public vhost, nginx does not proxy it |
| reads | `/srv/bitcal/data/current/events_{en,ru}.db`, mode `0444`, in a `0555` directory |
| built with | `CGO_ENABLED=1 go build -tags fts5`, **on Ubuntu/glibc** |

## Building

```sh
make build          # CGO_ENABLED=1 go build -tags fts5 -ldflags "-X main.version=$(VERSION)"
make test
make version        # what will be baked in and reported by /health
```

Two things about this build are not optional:

1.  **`-tags fts5`.** `gorm.io/driver/sqlite` uses `mattn/go-sqlite3`, which does not compile
    FTS5 in by default. Without the tag the binary builds and starts perfectly happily, and
    then every query touching `events_fts` fails at runtime with `no such module: fts5` — so
    `/api/search` returns 500 and nothing else complains.
2.  **Build on Ubuntu/glibc, or in a matching container.** CGO ties the binary to the C
    library it was built against. A binary built on Alpine (musl) or on a Mac will not start
    on the box at all. From any host with Docker, `make build-ubuntu` produces one that will.

## Installing

```sh
sudo install -o root -g root -m 0755 bitcal-api /srv/bitcal/bin/bitcal-api
sudo cp deploy/bitcal-api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now bitcal-api
```

The unit sets `LISTEN_ADDR`, `DB_PATH_EN`, `DB_PATH_RU` and `CORS_ALLOWED_ORIGINS`, and reads
`API_KEYS` from `/etc/bitcal/api.env` so the secret stays out of git. It also applies
`ProtectSystem=strict` and `ReadOnlyPaths=/srv/bitcal/data`, which makes the read-only rule
structural: even if a future code change tried to open a database read-write, the kernel
refuses.

## Environment variables

*   `API_KEYS` — **required**, comma-separated. The service exits if unset.
*   `DB_PATH_EN` — **required**, no default. The service exits if unset, naming the variable.
*   `DB_PATH_RU` — **required**, no default. Same.
*   `LISTEN_ADDR` — defaults to `127.0.0.1:3000`. A systemd unit cannot narrow a bind the app
    has already widened to `0.0.0.0`, so this default lives in the app.
*   `CORS_ALLOWED_ORIGINS` — comma-separated, defaults to `http://localhost:3000`.

There is no `PORT` variable any more; use `LISTEN_ADDR`.

## Releasing a new database artifact

The full procedure is recorded in the comment header of `deploy/bitcal-api.service`, next to
the unit it applies to. In short: copy the validated databases into a new timestamped release
directory, `chmod 0444` the files and `0555` the directory, flip the `current` symlink,
**restart the service** — SQLite holds an open file descriptor, so flipping the symlink alone
leaves the old inode being served — then `curl localhost:3000/health` and check the reported
hashes against the `SHA256SUMS` that shipped with the release.

There is no publish script yet; the steps are manual.

## Startup checks

Before it listens, the service opens each artifact and proves its full-text index works. It
refuses to start if the index is missing, empty, or unreadable.

That looks heavy-handed for one endpoint, and it is deliberate. A broken FTS index does not
produce an error — `/api/search` returns an empty result set, which is indistinguishable from
a query that genuinely matched nothing. The Telegram bot would post nothing and report
success. Every other endpoint would keep working. Nothing downstream can detect this, so
startup is the only place where it can be made loud.

An index that is merely *incomplete* is not fatal: the service starts, logs a warning naming
the counts, and reports `"status": "degraded"` on `/health`. Partial search beats no service.

```
Full-text index does not cover every row: search will return incomplete results
```

## Verifying a deployment

```sh
systemctl status bitcal-api
journalctl -u bitcal-api -n 50

curl -s localhost:3000/health | jq .
curl -s -H "X-API-KEY: $KEY" 'localhost:3000/api/events?month=8&day=9&limit=100&lang=ru' | jq .
curl -s -H "X-API-KEY: $KEY" 'localhost:3000/api/search?q=bitcoin&lang=en' | jq '.pagination.total'
```

`/health` is the one that matters: it names the resolved release path and the SHA-256 of each
file the process actually has open, and reports whether the whole corpus is searchable.

As a single assertion for a release check:

```sh
curl -sf localhost:3000/health | jq -e '
  .status == "ok" and (.databases | to_entries | all(.value.fts.consistent))'
```

## Troubleshooting

*   **`no such module: fts5`** — the binary was built without `-tags fts5`. This should be
    impossible: the build fails without the tag (`fts5_required.go`).
*   **Exits naming `events_fts`** — the artifact's full-text index is missing, empty or
    unreadable. The message says which. Do not work around it by removing the check: the
    symptom in production is search silently returning nothing. Rebuild the artifact and
    verify with `sqlite3 events_xx.db "INSERT INTO events_fts(events_fts) VALUES('integrity-check')"`
    before publishing.
*   **`"status": "degraded"`** — the index covers fewer rows than `events` holds; compare
    `fts.indexed` against `rows` per language. Search works but misses events. The artifact
    was published with a partly-built index.
*   **Exits at startup naming `DB_PATH_EN` or `DB_PATH_RU`** — the variable is unset. There
    are deliberately no defaults.
*   **`unable to open database file (14)`** — usually a WAL-mode artifact shipped without its
    sidecars. Check header bytes 18/19: `02 02` means WAL, `01 01` means delete mode. Fix the
    artifact. **Do not add `immutable=1`** to work around it — it bypasses locking, is false
    across a symlink flip, and converts a clean crash into silent wrong reads.
*   **`attempt to write a readonly database`** — something is issuing a write or DDL against
    the `0444` artifact. That is the failure working as designed; find the write.
*   **`/health` hashes do not match the release** — the running process is serving a different
    inode than you published, almost always a missing restart after the symlink flip.

[![⚡️zapmeacoffee](https://img.shields.io/badge/⚡️zap_-me_a_coffee-violet?style=plastic)](https://zapmeacoffee.com/npub1tcalvjvswjh5rwhr3gywmfjzghthexjpddzvlxre9wxfqz4euqys0309hn)
