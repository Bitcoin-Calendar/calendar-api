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
sudo install -o root -g root -m 0755 bitcal-api /srv/bitcal/api/bitcal-api
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

```sh
./deploy/publish-db.sh --dry-run    # every check, nothing staged or flipped
./deploy/publish-db.sh              # publish, with a confirmation prompt
```

`publish-db.sh` automates the runbook that is still recorded in the comment header of
`deploy/bitcal-api.service`. It:

1.  verifies `SHA256SUMS` against the source databases and runs `validate.py` on them;
2.  reports whether the artifacts are committed — scoped to the artifact paths, because the
    canonical repo shares a root with unrelated projects and is therefore always dirty;
3.  stages into `releases/<ts>.incoming`, verifies the checksums *after transfer*, sets
    `0444`/`0555` owned by `deploy`, then renames — so a publish that dies midway can never
    leave something that looks like a valid release;
4.  runs `validate.py` again **against the staged copy**, because the artifact is a copy and a
    copy step that opens a database read-write can leave a sidecar or flip the WAL header byte
    after the source already passed;
4b. pulls the staged databases back and runs `TestFixtureSchemaMatchesCanonical` against them,
    which is the check that stops a new canonical column shipping into an API that cannot emit
    it. `validate.py` asks whether the data is sound; this asks whether this service can
    express it, and nothing owned that question until `category` shipped invisibly. A failure
    here aborts **before** the symlink flip, so the running service is untouched; override with
    `--allow-schema-drift` if you have decided to publish data ahead of the binary;
5.  flips the symlink and restarts — the restart is mandatory, SQLite holds an open file
    descriptor and flipping alone leaves the old inode being served;
6.  verifies `/health` names the new release, that its hashes match what was published, that
    every row is indexed in both languages, and that the category vocabulary and the landmark
    flag are what the artifact carries;
7.  asserts search still returns hits for `биткоин` and `bitcoin` — the only check here that
    would catch a tokenizer change, and the Cyrillic one is the case that actually breaks;
8.  prunes old releases, keeping `--keep N` (default 5).

**Any failure after the symlink flip rolls back automatically** to the previous release and
restarts it. A rejected release directory is left in place for inspection.

### When a release adds a column, ship the binary first

`publish-api.sh` **then** `publish-db.sh`. Follow that order because it is free, not because
the other one breaks something.

Binary-first always works, by construction: the service is built to serve an artifact older
than itself. A new binary against an artifact lacking `category` or `landmark` boots, answers
every endpoint, renders the missing field as its zero value, and rejects only the filter it
genuinely cannot answer. That is the same property that lets a rollback work at all, and it is
covered by tests. So there is never a reason to reach for the other order.

Data-first is not an outage either — it just does nothing useful for a while. The new column
ships inside the artifact and reaches no response until the binary catches up, and no client
sees a wrong answer in the meantime, because nothing consumes this API today except the
Telegram bot, which reads neither column. The cost is that the flag is invisible and you may
not notice it is invisible.

That is worth one sentence of care because step 4b will not catch it. The schema guard runs
the API's test from the *local* repo, so it proves the staged artifact matches the checkout on
your laptop; it knows nothing about how old the binary on the box is, and passes green either
way. What does report it is `/health`: `publish-db.sh` warns when the running binary carries no
`categories` or no `landmark` block, meaning the binary predates the field — rather than
letting a silently skipped check read as a passing one.

The `deploy` user owns the artifacts but cannot be logged into (`nologin`, no home, no keys) —
it exists so that `bitcal` cannot write them. Publishing therefore connects as `root` and
chowns afterwards.

## Releasing a new binary

```sh
./deploy/publish-api.sh --dry-run   # every check, nothing built or installed
./deploy/publish-api.sh             # build, install, verify, with a prompt
```

`publish-api.sh` is the counterpart to `publish-db.sh` and their scopes do not overlap: that
one ships data and never rebuilds, this one ships a binary and never touches the data. It
automates the procedure that was walked by hand for `ec6b5de`, and it:

1.  refuses to build a dirty or unpushed tree — the binary is exported from `HEAD` with
    `git archive`, so uncommitted work would be absent from a binary whose version string
    names the commit as though it were there (`--allow-dirty` overrides);
2.  runs `make test` locally first, so a broken change costs six seconds rather than a
    round trip;
3.  exports `HEAD` to a temp directory **on the box** and builds it there. CGO ties the
    binary to the C library it was built against, so building on the target makes the glibc
    match true by construction rather than by remembering to use `make build-ubuntu`. Nothing
    persists: no source tree, no build output. `VERSION` is passed explicitly because the
    exported tree has no `.git` and the Makefile's fallback would silently yield
    `0.1.0-unknown`;
4.  runs `make test` **again on the box**, which is the layer that proves the `fts5` tag, the
    read-only open and the boot probe against the target's own libc;
5.  backs the running binary up to `bitcal-api.bak-<UTC timestamp>`, then *renames* the new
    one over it — writing over a busy executable in place fails with `ETXTBSY`, while a
    rename swaps the directory entry and leaves the running process on its old inode;
6.  restarts, watching `NRestarts` so a crash-loop is reported in seconds rather than at the
    timeout;
7.  verifies `/health` reports the new `version` **and that the database sha256s, row counts
    and release paths are unchanged**. A binary deploy must not move the data; if those
    hashes moved, the two release paths have collided and that is worth knowing immediately;
8.  sends one real search through the new binary, since `/health` does not exercise the query
    path or the FTS module;
9.  prunes old `.bak` binaries, keeping `--keep N` (default 5).

**Any failure after the install rolls back automatically** by restoring the `.bak` binary and
restarting.

The two paths are independent, so the box can serve new data on an old binary or the reverse.
That is allowed — on 2026-08-09 it did exactly that for about 35 minutes — but when
diagnosing anything, check both halves of `/health`.

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

Deliberately no assertion on `categories.count` or `landmark.count` here. Both are properties
of the data, not of the deployment: the vocabulary went from 15 values to 8 on 2026-08-12 and
the landmark count moves whenever the owner re-judges a row, so a number pinned in a release
check is a check that fails for the wrong reason. Assert `present`, or that the count is
non-zero, and read the counts `publish-db.sh` prints.

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
