#!/usr/bin/env bash
#
# publish-db.sh — ship the canonical database artifacts to the box.
#
# Automates the runbook in the comment header of bitcal-api.service. That
# procedure was walked by hand for release 20260809T092607Z before this was
# written, which is why the steps below are in the order they are.
#
#   local  preflight  checksums + validate.py + scoped git state
#   remote stage      copy into releases/<ts>.incoming, verify, permission, rename
#   remote validate   run validate.py against the STAGED copy, not just the source
#   remote flip       symlink, then restart (mandatory — see below)
#   remote verify     /health hashes, FTS coverage, and a vocabulary assertion
#   remote prune      keep the last N releases
#
# Any failure after the flip rolls back to the previous release automatically.
#
# The restart is not optional. SQLite holds an open file descriptor on the
# database inode; flipping `current` alone leaves the old file being served,
# with /health cheerfully reporting the old hash. Verified on the box:
#
#   /proc/<pid>/fd/3 -> /srv/bitcal/data/releases/20260809T092607Z/events_en.db
#
# Why the staged copy is validated a second time: passing validate.py on the
# laptop proves the *source* is clean, and the artifact is a copy. A copy step
# that opens the database read-write — some tools do, to "verify" it — leaves a
# sidecar or flips the WAL header byte after validation already passed. The
# second run is against the bytes that will actually be served.
#
# Usage:
#   ./publish-db.sh                    # publish, with a confirmation prompt
#   ./publish-db.sh --dry-run          # every check, no staging, no flip
#   ./publish-db.sh --yes              # no prompt (for a future cron/CI caller)
#   ./publish-db.sh --keep 10
#   ./publish-db.sh --source /path/to/calendar/dbs --host root@example
#
set -euo pipefail

# --- defaults ---------------------------------------------------------------

SOURCE_DIR="${BITCAL_SOURCE:-/Users/tony/code/dump/projects/21ideas/calendar/dbs}"
SSH_HOST="${BITCAL_HOST:-root@169.58.80.219}"
REMOTE_DATA="/srv/bitcal/data"
SERVICE="bitcal-api"
HEALTH_URL="http://127.0.0.1:3000/health"
API_URL="http://127.0.0.1:3000/api"
KEEP=5
DRY_RUN=0
ASSUME_YES=0

DBS=(events_ru.db events_en.db)

# Vocabulary assertion: language, query term, and the count observed at the
# last release. A change here is not fatal — the corpus grows — but it is
# printed loudly, because a *drop to zero* means the FTS tokenizer stopped
# handling that script, which nothing else in this script would catch.
# Cyrillic is the one that actually exercises the tokenizer.
VOCAB_RU_TERM="биткоин"
VOCAB_EN_TERM="bitcoin"
VOCAB_RU_LAST=246
VOCAB_EN_LAST=450

# --- argument parsing -------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
	--source) SOURCE_DIR="$2"; shift 2 ;;
	--host) SSH_HOST="$2"; shift 2 ;;
	--keep) KEEP="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	-h | --help) sed -n '2,40p' "$0"; exit 0 ;;
	*) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
	esac
done

case "$KEEP" in
'' | *[!0-9]*) echo "--keep needs a number, got: $KEEP" >&2; exit 2 ;;
esac
[ "$KEEP" -ge 1 ] || { echo "--keep must be at least 1" >&2; exit 2; }

# --- output helpers ---------------------------------------------------------

if [ -t 1 ]; then
	BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m')
	GREEN=$(printf '\033[32m'); YELLOW=$(printf '\033[33m'); OFF=$(printf '\033[0m')
else
	BOLD=""; RED=""; GREEN=""; YELLOW=""; OFF=""
fi

step() { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$OFF"; }
ok()   { printf '    %sok%s   %s\n' "$GREEN" "$OFF" "$*"; }
warn() { printf '    %swarn%s %s\n' "$YELLOW" "$OFF" "$*"; }
die()  { printf '\n%serror%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

# One SSH connection, reused. Without multiplexing this script pays a full
# handshake per step — and the health poll below would pay one per attempt,
# which is what made an early version take minutes to notice a failed boot.
SSH_CTL_DIR=$(mktemp -d "${TMPDIR:-/tmp}/bitcal-ssh.XXXXXX")
SSH_OPTS=(-o BatchMode=yes -o ControlMaster=auto -o ControlPath="$SSH_CTL_DIR/ctl" -o ControlPersist=180)

remote() { ssh "${SSH_OPTS[@]}" "$SSH_HOST" "$@"; }
copy_to() { scp -q "${SSH_OPTS[@]}" "$@"; }

close_ssh() {
	ssh "${SSH_OPTS[@]}" -O exit "$SSH_HOST" >/dev/null 2>&1 || true
	rm -rf "$SSH_CTL_DIR"
}
trap close_ssh EXIT

# sha256 tooling differs between macOS and Linux; both understand -c.
if command -v shasum >/dev/null 2>&1; then
	SHA_CHECK() { shasum -a 256 -c "$@"; }
elif command -v sha256sum >/dev/null 2>&1; then
	SHA_CHECK() { sha256sum -c "$@"; }
else
	die "neither shasum nor sha256sum found"
fi

# ---------------------------------------------------------------------------
# 1. Local preflight
# ---------------------------------------------------------------------------

step "Local preflight"

[ -d "$SOURCE_DIR" ] || die "source directory not found: $SOURCE_DIR"

VALIDATOR="$(cd "$SOURCE_DIR/.." && pwd)/validate.py"
BASELINE="$(dirname "$VALIDATOR")/validate-baseline.json"
[ -f "$VALIDATOR" ] || die "validate.py not found next to the source: $VALIDATOR"

for f in "${DBS[@]}" SHA256SUMS; do
	[ -f "$SOURCE_DIR/$f" ] || die "missing from source: $SOURCE_DIR/$f"
done
ok "source: $SOURCE_DIR"

command -v python3 >/dev/null 2>&1 || die "python3 is required to run validate.py"

# Checksums first: everything downstream trusts SHA256SUMS, so if it is stale
# nothing else means anything.
( cd "$SOURCE_DIR" && SHA_CHECK SHA256SUMS >/dev/null 2>&1 ) \
	|| die "SHA256SUMS does not match the databases in $SOURCE_DIR.
       Regenerate it, or find out which file changed — do not publish past this."
ok "SHA256SUMS matches the source databases"

# The validator covers the two artifact invariants (WAL header, sidecars) as
# its invariant 12, plus the FTS index/trigger/docsize checks as invariant 9.
# Deliberately not reimplemented here.
if ! VALIDATE_OUT=$(cd "$(dirname "$VALIDATOR")" && python3 "$VALIDATOR" "$SOURCE_DIR/events_ru.db" "$SOURCE_DIR/events_en.db" 2>&1); then
	printf '%s\n' "$VALIDATE_OUT"
	die "validate.py rejected the source databases"
fi
printf '%s\n' "$VALIDATE_OUT" | sed 's/^/       /'
ok "validate.py: PASS on the source"

# Scoped to the artifacts, not the repo. The canonical repo root is a large
# monorepo (~/code/dump) shared with unrelated projects, so a bare
# `git status` is always dirty and would make this check meaningless noise.
SOURCE_GIT_REV="unknown"
if git -C "$SOURCE_DIR" rev-parse --git-dir >/dev/null 2>&1; then
	SOURCE_GIT_REV=$(git -C "$SOURCE_DIR" rev-parse --short HEAD)
	DIRTY=$(git -C "$SOURCE_DIR" status --porcelain -- "$SOURCE_DIR" "$VALIDATOR" 2>/dev/null || true)
	if [ -n "$DIRTY" ]; then
		warn "the artifacts are not committed (rev $SOURCE_GIT_REV plus local changes):"
		printf '%s\n' "$DIRTY" | sed 's/^/         /'
		warn "publishing anyway — the manifest records this, but the release will not be reproducible from git"
	else
		ok "artifacts committed at $SOURCE_GIT_REV"
	fi
fi

# Row and index counts, for the manifest and the confirmation prompt.
declare -a SUMMARY=()
for db in "${DBS[@]}"; do
	counts=$(python3 - "$SOURCE_DIR/$db" <<'PY'
import sqlite3, sys
con = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
rows = con.execute("SELECT count(*) FROM events").fetchone()[0]
idx = con.execute("SELECT count(*) FROM events_fts_docsize").fetchone()[0]
print(f"{rows} {idx}")
PY
	)
	SUMMARY+=("$db $counts")
done

# ---------------------------------------------------------------------------
# 2. Remote preflight — capture what is live now, so a rollback has a target
# ---------------------------------------------------------------------------

step "Remote preflight ($SSH_HOST)"

remote true 2>/dev/null || die "cannot reach $SSH_HOST over ssh"
ok "ssh reachable"

remote "systemctl cat $SERVICE >/dev/null 2>&1" \
	|| die "$SERVICE is not installed on $SSH_HOST"

PREVIOUS_RELEASE=$(remote "readlink -f $REMOTE_DATA/current 2>/dev/null || true")
if [ -n "$PREVIOUS_RELEASE" ]; then
	ok "currently serving: $(basename "$PREVIOUS_RELEASE")"
else
	warn "no current release — this is the first publish"
fi

remote "python3 -c 'import sqlite3' >/dev/null 2>&1" \
	|| die "python3 with the sqlite3 module is required on the box to validate the staged copy"

# The box has no sqlite3 CLI, which is why every remote database assertion in
# this script goes through python3 or /health rather than shelling out to it.

AVAIL_KB=$(remote "df -Pk $REMOTE_DATA | awk 'NR==2 {print \$4}'")
# Only the files that ship. $SOURCE_DIR also holds superseded/, which is an
# order of magnitude larger and would make this estimate meaningless.
NEED_KB=$(( $(du -sk "${DBS[@]/#/$SOURCE_DIR/}" | awk '{s += $1} END {print s}') + 51200 ))
[ "$AVAIL_KB" -gt "$NEED_KB" ] \
	|| die "not enough space on the box: ${AVAIL_KB}KB free, need ~${NEED_KB}KB"
ok "disk: $(( AVAIL_KB / 1024 ))MB free"

# ---------------------------------------------------------------------------
# 3. Confirm
# ---------------------------------------------------------------------------

RELEASE=$(date -u +%Y%m%dT%H%M%SZ)
STAGING="$REMOTE_DATA/releases/${RELEASE}.incoming"
TARGET="$REMOTE_DATA/releases/$RELEASE"

step "About to publish"
printf '    release      %s\n' "$RELEASE"
printf '    source       %s (git %s)\n' "$SOURCE_DIR" "$SOURCE_GIT_REV"
printf '    host         %s\n' "$SSH_HOST"
printf '    replacing    %s\n' "${PREVIOUS_RELEASE:-<none>}"
for line in "${SUMMARY[@]}"; do
	set -- $line
	printf '    %-14s %s rows, %s indexed\n' "$1" "$2" "$3"
done

if [ "$DRY_RUN" -eq 1 ]; then
	step "Dry run — every check above passed; nothing was staged, flipped or restarted"
	exit 0
fi

if [ "$ASSUME_YES" -eq 0 ]; then
	printf '\n    Publish? [y/N] '
	read -r reply
	case "$reply" in
	y | Y | yes | YES) ;;
	*) echo "    aborted"; exit 1 ;;
	esac
fi

# ---------------------------------------------------------------------------
# 4. Stage
# ---------------------------------------------------------------------------
# Staged under a .incoming name and renamed only once complete and verified.
# A publish that dies midway therefore never leaves a directory that looks
# like a valid release — the symlink flip can only ever target a finished one.

step "Staging $RELEASE"

remote "test ! -e '$TARGET' && test ! -e '$STAGING'" \
	|| die "$TARGET or $STAGING already exists — refusing to overwrite a release"

remote "mkdir -p '$STAGING'"

cleanup_staging() {
	[ -n "${STAGING:-}" ] || return 0
	remote "chmod -R u+w '$STAGING' 2>/dev/null || true; rm -rf '$STAGING'" 2>/dev/null || true
}

copy_to "$SOURCE_DIR/events_ru.db" "$SOURCE_DIR/events_en.db" "$SOURCE_DIR/SHA256SUMS" \
	"$SSH_HOST:$STAGING/" || { cleanup_staging; die "copy failed"; }
ok "copied $(printf '%s, ' "${DBS[@]}" | sed 's/, $//') and SHA256SUMS"

# Did the bytes survive the wire?
remote "cd '$STAGING' && sha256sum -c SHA256SUMS" >/dev/null 2>&1 \
	|| { cleanup_staging; die "checksums do not match after transfer — the copy is corrupt"; }
ok "checksums verified on the box"

# A manifest, so a future reader can tell where a release came from without
# reconstructing it from shell history.
remote "cat > '$STAGING/RELEASE.txt'" <<EOF
release:     $RELEASE
published:   $(date -u +%Y-%m-%dT%H:%M:%SZ) by $(whoami)@$(hostname -s)
source:      $SOURCE_DIR
source git:  $SOURCE_GIT_REV
$(for line in "${SUMMARY[@]}"; do set -- $line; printf '%-12s %s rows, %s indexed\n' "$1:" "$2" "$3"; done)
replaces:    ${PREVIOUS_RELEASE:-<none>}
EOF

# Permissions are the actual enforcement of the read-only rule: files readable
# by everyone and writable by no one, in a directory the service user cannot
# write either.
remote "chown deploy:deploy '$STAGING'/* && chmod 0444 '$STAGING'/* && chown deploy:deploy '$STAGING' && chmod 0555 '$STAGING'" \
	|| { cleanup_staging; die "could not set ownership/permissions"; }
ok "staged 0444 in a 0555 directory, owned deploy:deploy"

remote "mv -T '$STAGING' '$TARGET'" || { cleanup_staging; die "could not finalise the release directory"; }
STAGING=""
ok "finalised as $TARGET"

# ---------------------------------------------------------------------------
# 5. Validate the staged copy
# ---------------------------------------------------------------------------

step "Validating the staged copy"

REMOTE_TMP=$(remote "mktemp -d /tmp/bitcal-validate.XXXXXX")
copy_to "$VALIDATOR" "$SSH_HOST:$REMOTE_TMP/validate.py"
# The baseline must travel with it: validate.py resolves it next to its own
# file, and without it every baselined known-open item reads as a new
# regression and the run fails for the wrong reason.
if [ -f "$BASELINE" ]; then
	copy_to "$BASELINE" "$SSH_HOST:$REMOTE_TMP/validate-baseline.json"
else
	warn "no validate-baseline.json beside the validator; known-open items may read as regressions"
fi

if ! STAGED_OUT=$(remote "cd '$REMOTE_TMP' && python3 validate.py '$TARGET/events_ru.db' '$TARGET/events_en.db' 2>&1"); then
	printf '%s\n' "$STAGED_OUT" | sed 's/^/       /'
	remote "rm -rf '$REMOTE_TMP'"
	die "validate.py rejected the STAGED copy at $TARGET.
       The source passed, so the copy introduced this. The release directory has
       been left in place for inspection; the symlink was NOT flipped and the
       running service is untouched."
fi
remote "rm -rf '$REMOTE_TMP'"
printf '%s\n' "$STAGED_OUT" | sed 's/^/       /'
ok "validate.py: PASS on the staged artifact"

# ---------------------------------------------------------------------------
# 6. Flip and restart
# ---------------------------------------------------------------------------

step "Flipping the symlink and restarting"

rollback() {
	[ -n "$PREVIOUS_RELEASE" ] || {
		warn "no previous release to roll back to; $SERVICE is left stopped or unhealthy"
		return
	}
	printf '\n%s==> Rolling back to %s%s\n' "$BOLD" "$(basename "$PREVIOUS_RELEASE")" "$OFF"
	# reset-failed first: a crash-looping unit can be in a state where systemd
	# has given up on it, and then `restart` alone leaves it stopped.
	remote "ln -sfn '$PREVIOUS_RELEASE' $REMOTE_DATA/current.new \
		&& mv -T $REMOTE_DATA/current.new $REMOTE_DATA/current \
		&& systemctl reset-failed $SERVICE 2>/dev/null; systemctl restart $SERVICE" || true

	if [ "$(remote "bash -s" -- "$HEALTH_URL" 30 <<'REMOTE' || true
set -u
url=$1; end=$(( $(date +%s) + $2 ))
while [ "$(date +%s)" -lt "$end" ]; do
	curl -sf --max-time 3 "$url" >/dev/null 2>&1 && { echo HEALTHY; exit 0; }
	sleep 0.5
done
echo DOWN
REMOTE
	)" = "HEALTHY" ]; then
		ok "rolled back; $(basename "$PREVIOUS_RELEASE") is serving again"
	else
		warn "ROLLBACK DID NOT COME UP — the service needs attention now"
		warn "  ssh $SSH_HOST 'journalctl -u $SERVICE -n 50 --no-pager'"
		remote "journalctl -u $SERVICE -n 30 --no-pager" 2>/dev/null | sed 's/^/       /' || true
	fi
}

# ln -sfn onto a temporary name then mv -T: a rename over an existing symlink
# is atomic, so no request ever observes a missing `current`.
remote "ln -sfn '$TARGET' $REMOTE_DATA/current.new && mv -T $REMOTE_DATA/current.new $REMOTE_DATA/current" \
	|| die "symlink flip failed; nothing was restarted and the old release is still serving"
ok "current -> $RELEASE"

# NRestarts before the manual restart. A manual `systemctl restart` does not
# increment it, so any increase afterwards means systemd is auto-restarting a
# process that keeps dying — the artifact was rejected. That is the signal the
# poll below watches for, and it is why this reports a bad release in seconds
# rather than waiting out the whole timeout.
RESTARTS_BEFORE=$(remote "systemctl show $SERVICE -p NRestarts --value 2>/dev/null || echo 0")

remote "systemctl restart $SERVICE" || { rollback; die "restart failed"; }

# The poll runs remotely, in one SSH session. Polling from here would pay a
# full SSH handshake per attempt — an earlier version did, and took over two
# minutes to notice a boot failure that systemd had already diagnosed twice.
step "Waiting for the service"
START=$(date +%s)
WAIT_RESULT=$(remote "bash -s" -- "$SERVICE" "$HEALTH_URL" "$RESTARTS_BEFORE" 45 <<'REMOTE' || true
set -u
service=$1; url=$2; baseline=$3; timeout=$4
end=$(( $(date +%s) + timeout ))
while [ "$(date +%s)" -lt "$end" ]; do
	if curl -sf --max-time 3 "$url" >/dev/null 2>&1; then
		echo HEALTHY
		exit 0
	fi
	now=$(systemctl show "$service" -p NRestarts --value 2>/dev/null || echo 0)
	if [ "${now:-0}" -gt "${baseline:-0}" ]; then
		echo CRASHLOOP
		exit 0
	fi
	sleep 0.5
done
echo TIMEOUT
REMOTE
)
ELAPSED=$(( $(date +%s) - START ))

if [ "$WAIT_RESULT" != "HEALTHY" ]; then
	case "$WAIT_RESULT" in
	CRASHLOOP) warn "the service is crash-looping on the new artifact (${ELAPSED}s)" ;;
	*) warn "the service did not answer /health within ${ELAPSED}s" ;;
	esac
	remote "journalctl -u $SERVICE -n 25 --no-pager" 2>/dev/null | sed 's/^/       /' || true
	rollback
	die "publish failed: the new artifact did not come up.
       If the log names events_fts, the index in this artifact is unusable and
       the boot probe refused it — which is the probe working, not a bug.
       The rejected release was left at $TARGET for inspection."
fi
ok "healthy after ${ELAPSED}s"

# ---------------------------------------------------------------------------
# 7. Verify what is actually being served
# ---------------------------------------------------------------------------

step "Verifying"

HEALTH=$(remote "curl -s --max-time 10 $HEALTH_URL")

# Parsed locally with python3 — already a hard dependency for validate.py —
# rather than by piping into a remote jq per field. One round trip, one place
# where a mismatch is decided, and no shell-quoting a jq program through ssh.
#
# Asserts, in one pass: status is ok; both languages resolve to the release
# just published (this is the check that catches a missed restart); the hashes
# the process reports match the checksums that shipped with the artifact; and
# every row is indexed in both languages.
if ! VERIFY_OUT=$(python3 - "$TARGET" "$SOURCE_DIR/SHA256SUMS" "$HEALTH" <<'PY'
import json, sys, pathlib

target, sums_path = sys.argv[1], sys.argv[2]
health = json.loads(sys.argv[3])

published = {}
for line in pathlib.Path(sums_path).read_text().splitlines():
    if line.strip():
        digest, name = line.split()[0], line.split()[-1]
        published[name] = digest

problems, notes = [], []
if health.get("status") != "ok":
    problems.append(f'status is {health.get("status")!r}, want "ok"')

for lang in ("en", "ru"):
    db = health.get("databases", {}).get(lang)
    if db is None:
        problems.append(f"{lang}: absent from /health")
        continue

    name = db["path"].rsplit("/", 1)[-1]
    if not db["path"].startswith(target + "/"):
        problems.append(f'{lang}: serving {db["path"]}, not the release just published')

    want = published.get(name)
    if want is None:
        problems.append(f"{lang}: {name} has no line in SHA256SUMS")
    elif want != db["sha256"]:
        problems.append(
            f'{lang}: the process has a different file open than was published\n'
            f'         published {want}\n'
            f'         serving   {db["sha256"]}')

    fts = db.get("fts", {})
    if not fts.get("consistent") or fts.get("indexed") != db["rows"]:
        problems.append(
            f'{lang}: index covers {fts.get("indexed")} of {db["rows"]} rows')
    else:
        notes.append(f'{lang}: {db["rows"]} rows, all indexed, sha256 matches')

print("\n".join(notes))
if problems:
    print("PROBLEMS", file=sys.stderr)
    print("\n".join("  " + p for p in problems), file=sys.stderr)
    sys.exit(1)
PY
); then
	printf '%s\n' "$VERIFY_OUT" | sed 's/^/       /'
	printf '%s\n' "$HEALTH" | sed 's/^/       /'
	rollback
	die "/health does not describe the release that was just published"
fi
printf '%s\n' "$VERIFY_OUT" | while IFS= read -r line; do [ -n "$line" ] && ok "$line"; done

# Vocabulary assertion. The strongest single end-to-end signal, and the only
# one here that would catch a tokenizer change: it goes through the real search
# endpoint rather than /health. Cyrillic is the case that actually breaks.
step "Vocabulary"
API_KEY=$(remote "sed -n 's/^API_KEYS=//p' /etc/bitcal/api.env | cut -d, -f1")
[ -n "$API_KEY" ] || warn "could not read an API key from /etc/bitcal/api.env; skipping the search assertion"

if [ -n "$API_KEY" ]; then
	vocab_failed=0
	for pair in "ru:$VOCAB_RU_TERM:$VOCAB_RU_LAST" "en:$VOCAB_EN_TERM:$VOCAB_EN_LAST"; do
		lang=${pair%%:*}; rest=${pair#*:}
		term=${rest%:*}; last=${rest##*:}
		total=$(remote "curl -s --max-time 10 -H 'X-API-KEY: $API_KEY' --get \
			--data-urlencode 'q=$term' --data-urlencode 'lang=$lang' \
			'$API_URL/search' | jq -r '.pagination.total // 0'")
		if [ "$total" -eq 0 ]; then
			warn "$lang: '$term' matched NOTHING (was $last) — the index or the tokenizer is broken for this script"
			vocab_failed=1
		elif [ "$total" -ne "$last" ]; then
			upper=$(printf '%s' "$lang" | tr '[:lower:]' '[:upper:]')
			ok "$lang: '$term' -> $total (was $last; update VOCAB_${upper}_LAST in this script if intended)"
		else
			ok "$lang: '$term' -> $total"
		fi
	done
	if [ "$vocab_failed" -eq 1 ]; then
		rollback
		die "a language returned no search results at all — not publishing this"
	fi
fi

# ---------------------------------------------------------------------------
# 8. Prune
# ---------------------------------------------------------------------------

step "Pruning old releases (keeping $KEEP)"

PRUNED=$(remote "bash -s" -- "$REMOTE_DATA" "$KEEP" <<'REMOTE'
set -euo pipefail
data="$1"; keep="$2"
current=$(readlink -f "$data/current")
cd "$data/releases" || exit 0
# Names are UTC timestamps, so lexical order is chronological.
mapfile -t all < <(find . -maxdepth 1 -mindepth 1 -type d -printf '%f\n' | sort)
count=${#all[@]}
[ "$count" -le "$keep" ] && exit 0
for d in "${all[@]:0:$((count - keep))}"; do
	full="$data/releases/$d"
	# Never remove what is being served, whatever the arithmetic says.
	[ "$full" = "$current" ] && continue
	case "$full" in
	"$data/releases/"*) chmod -R u+w "$full" && rm -rf "$full" && echo "$d" ;;
	esac
done
REMOTE
)
if [ -n "$PRUNED" ]; then
	printf '%s\n' "$PRUNED" | sed 's/^/       removed /'
else
	ok "nothing to prune"
fi

REMAINING=$(remote "ls -1 $REMOTE_DATA/releases | wc -l | tr -d ' '")

# ---------------------------------------------------------------------------

step "Published $RELEASE"
printf '    %s releases retained, serving %s\n' "$REMAINING" "$RELEASE"
printf '    verify by hand: ssh %s \"curl -s %s | jq .\"\n' "$SSH_HOST" "$HEALTH_URL"
