#!/usr/bin/env bash
#
# publish-db.sh — ship the canonical database artifacts to the box.
#
# Automates the runbook in the comment header of bitcal-api.service. That
# procedure was walked by hand for release 20260809T092607Z before this was
# written, which is why the steps below are in the order they are.
#
#   local  preflight  checksums + validate.py + scoped git state
#   remote preflight  rollback target, disk, and that the box can run the checks
#   remote stage      copy into releases/<ts>.incoming, verify, permission, rename
#   remote validate   run validate.py against the STAGED copy, not just the source
#   local  schema     the API's own test, against the staged copy — see below
#   remote flip       symlink, then restart (mandatory — see below)
#   remote verify     /health hashes, FTS coverage, categories, landmarks, and a
#                     search assertion
#   remote prune      keep the last N releases
#
# What happens when something fails after the flip depends on what failed, and
# the distinction is the point:
#
#   the release is bad          roll back, automatically. A service that will
#                               not come up, a /health that describes a
#                               different release, a search term that matches
#                               nothing — the artifact is the problem.
#   the *check* could not run   do not roll back. An unreachable box or a
#                               missing jq says nothing about the release, and
#                               reverting a publish that may be perfectly good
#                               because the checker broke is the wrong trade.
#                               The script exits non-zero saying the release is
#                               live and unverified, and prints the commands to
#                               check or undo it by hand.
#
# It cannot promise more than that: rolling back needs SSH too, so a box that
# has gone away cannot be rolled back to anything by this or any other script.
# What it does promise is that it never exits quietly. Preflight checks the
# tooling the later steps depend on — jq, a service already answering /health —
# before anything is staged, so the second case is rare: a missing jq is a
# property of the box, not something that appears halfway through a publish.
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
# The schema guard is why this script knows about `go test`. validate.py checks
# that the data is internally consistent; it has no opinion about whether this
# API can express it. Canonical gained a mandatory `category` column on
# 2026-08-09, the Event struct did not, and the column shipped inside the
# artifact and reached no response — with validate.py passing and `make test`
# green throughout, because the suite's own fixture had no such column either.
# TestFixtureSchemaMatchesCanonical compares the fixture's schema against a real
# artifact, so pointing it at the staged copy turns "someone notices the schema
# changed" into a check that fires at the one moment a schema change is expected
# to be reviewed: release. It runs before the flip, so a failure costs nothing —
# the running service is untouched.
#
# Usage:
#   ./publish-db.sh                    # publish, with a confirmation prompt
#   ./publish-db.sh --dry-run          # every check, no staging, no flip
#   ./publish-db.sh --yes              # no prompt (for a future cron/CI caller)
#   ./publish-db.sh --keep 10
#   ./publish-db.sh --source /path/to/calendar/dbs --host root@example
#   ./publish-db.sh --allow-schema-drift   # publish data the API cannot yet emit
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
ALLOW_SCHEMA_DRIFT=0

# Read from the box during preflight, and empty when it could not be read — in
# which case the search assertion is skipped rather than guessed at.
API_KEY=""

# The repo this script lives in, so the schema guard can find the Go suite
# regardless of where it was invoked from.
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

DBS=(events_ru.db events_en.db)

# Vocabulary assertion: language, query term, and the count observed at the
# last release. A change here is not fatal — the corpus grows — but it is
# printed loudly, because a *drop to zero* means the FTS tokenizer stopped
# handling that script, which nothing else in this script would catch.
# Cyrillic is the one that actually exercises the tokenizer.
#
# Both numbers were re-measured against release 20260810T131954Z on 2026-08-10
# and both had drifted. EN fell 450 -> 389 because the `bitcoin` tag was retired
# in canonical: events_fts indexes the tags column, so removing the tag from
# ~76% of rows removed those matches. RU fell 246 -> 244 with ordinary edits.
# The previous release printed the warning below exactly as designed and the
# constants were simply never updated — which is the failure mode this comment
# exists to make harder.
#
# Re-measured again on 2026-08-13, against canonical after the step40 taxonomy
# migration, and both are UNCHANGED: ru 244, en 389. That step rewrote `category`
# on all 1,146 rows and added `landmark`, and touched neither title, description
# nor tags — the only three columns events_fts indexes — so the counts could not
# move. Confirmed by running both terms against dbs/superseded/*pre-step40* and
# getting the same two numbers. Note that `bitcoin` is no longer a category at
# all: step40 dissolved it, so it is now neither a tag nor a category, and an
# earlier version of this comment offering it as the survivor of that 450 -> 389
# drop is no longer true.
VOCAB_RU_TERM="биткоин"
VOCAB_EN_TERM="bitcoin"
VOCAB_RU_LAST=244
VOCAB_EN_LAST=389

# --- argument parsing -------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
	--source) SOURCE_DIR="$2"; shift 2 ;;
	--host) SSH_HOST="$2"; shift 2 ;;
	--keep) KEEP="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	--allow-schema-drift) ALLOW_SCHEMA_DRIFT=1; shift ;;
	# Every comment line after the shebang, stopping at the first line of code.
	# A hardcoded line range silently truncates the help the next time the
	# header grows, which is what happened when the header was rewritten and
	# this kept printing only as far as the old line 49.
	-h | --help) awk 'NR>1 && /^#/ {print; next} NR>1 {exit}' "$0"; exit 0 ;;
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
# The category vocabulary, which validate.py resolves as
# `Path(__file__).parent / "categories.json"` and refuses to run without —
# deliberately, since validating against an empty set would make every category
# legal. It became a dependency on 2026-08-12, when the vocabulary stopped being
# a literal in the validator and moved to a file the validator and the website
# both read.
#
# Checked here rather than where it is copied, because that is the whole point
# of this step: it is a property of the source tree, so discovering it costs
# nothing now and costs a staged-but-unpublishable release later. That is not
# hypothetical — release 20260813T054029Z staged, copied, checksummed, and then
# failed remote validation with `categories.json is missing`, because this
# script shipped the validator without it.
CATEGORY_FILE="$(dirname "$VALIDATOR")/categories.json"
[ -f "$VALIDATOR" ] || die "validate.py not found next to the source: $VALIDATOR"
[ -f "$CATEGORY_FILE" ] || die "categories.json not found next to the validator: $CATEGORY_FILE
       validate.py reads the category vocabulary from it and will not run without
       it. Regenerate it in the canonical repo with: ruby tools/build-categories.rb"

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

# Everything the verify step needs, proved before anything is staged.
#
# These are properties of the box, not of the release: jq either is installed or
# is not, and the service either is answering or is not, and neither changes
# halfway through a publish. Discovering them here costs nothing — nothing has
# been copied, no symlink has moved — and the operator gets "install jq on the
# box" instead of a rolled-back release and a verify step that could not run.
remote "command -v jq >/dev/null 2>&1" \
	|| die "jq is not installed on $SSH_HOST, and the search assertion at the end of this
       script needs it. Install it (apt-get install -y jq) and re-run. Publishing
       without it would mean flipping the symlink and then discovering that the
       release cannot be verified."
ok "jq present, the search assertion can parse a reply"

# Only when something is already being served. On a first publish there is
# nothing to answer, and requiring it would make the script unable to bootstrap.
if [ -n "$PREVIOUS_RELEASE" ]; then
	remote "curl -sf --max-time 5 $HEALTH_URL >/dev/null 2>&1" \
		|| die "$SERVICE is not answering $HEALTH_URL, so it is unhealthy before this
       script has touched anything. Fix that first: a rollback needs a service
       that can come back up, and a verify step needs one that can answer."
	ok "service answering /health"
fi

# Read here rather than after the flip for the same reason. It is also the one
# remote read whose failure used to be indistinguishable from an empty file.
if ! API_KEY=$(remote "sed -n 's/^API_KEYS=//p' /etc/bitcal/api.env | cut -d, -f1"); then
	API_KEY=""
fi
if [ -z "$API_KEY" ]; then
	warn "could not read an API key from /etc/bitcal/api.env; the search assertion will be skipped"
else
	ok "api key readable, search assertion will run"
fi

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
# Both of the validator's siblings must travel with it, because it resolves each
# one next to its own file and it is running from a temp directory on the box.
#
# They are not equally important, which is why one warns and one is already a
# hard failure in preflight. Without the baseline, every baselined known-open
# item reads as a new regression and the run fails for the wrong reason — bad,
# but the validator still runs. Without categories.json it does not start at
# all.
copy_to "$CATEGORY_FILE" "$SSH_HOST:$REMOTE_TMP/categories.json" \
	|| die "could not copy categories.json to the box; validate.py cannot run without it"
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
# 5b. Schema guard — can this API actually express what is being published?
# ---------------------------------------------------------------------------
# validate.py answers "is this data sound"; this answers "can the service emit
# it". They are different questions and the second one had no owner, which is
# how a mandatory column shipped into production and reached no response.
#
# Pulled back rather than checked in place: the box has no Go toolchain and no
# source tree, and it should stay that way. The file that comes back is the one
# that will be served, so the copy is the point rather than an inconvenience.

step "Schema guard (the API's own test, against the staged copy)"

schema_guard_failed=0
if ! command -v go >/dev/null 2>&1; then
	if [ "$ALLOW_SCHEMA_DRIFT" -eq 1 ]; then
		warn "go not found; skipping the schema guard because --allow-schema-drift was given"
	else
		die "go not found on PATH, so the schema guard cannot run.
       This check is what stops a new canonical column shipping into an API that
       cannot emit it. Install Go, or re-run with --allow-schema-drift if you
       have decided to publish data ahead of the service."
	fi
elif [ ! -d "$REPO_ROOT/tests" ]; then
	warn "no tests/ directory under $REPO_ROOT; skipping the schema guard"
else
	SCHEMA_TMP=$(mktemp -d "${TMPDIR:-/tmp}/bitcal-schema.XXXXXX")
	trap 'rm -rf "$SCHEMA_TMP"; close_ssh' EXIT

	for db in "${DBS[@]}"; do
		scp -q "${SSH_OPTS[@]}" "$SSH_HOST:$TARGET/$db" "$SCHEMA_TMP/$db" \
			|| die "could not fetch $db back from the staged release for the schema check"

		# Column sets must match; column *order* legitimately differs between
		# the two languages, so the test compares by name and both files are
		# expected to pass.
		if GUARD_OUT=$(cd "$REPO_ROOT" && BITCAL_CANONICAL_DB="$SCHEMA_TMP/$db" \
			CGO_ENABLED=1 go test -tags fts5 -count=1 \
			-run TestFixtureSchemaMatchesCanonical ./tests 2>&1); then
			ok "$db: schema matches what the API models"
		else
			printf '%s\n' "$GUARD_OUT" | sed 's/^/       /'
			schema_guard_failed=1
		fi
	done
	rm -rf "$SCHEMA_TMP"
	trap close_ssh EXIT
fi

if [ "$schema_guard_failed" -eq 1 ]; then
	if [ "$ALLOW_SCHEMA_DRIFT" -eq 1 ]; then
		warn "the staged artifact's schema does not match the API's model, and"
		warn "--allow-schema-drift was given — publishing anyway. The columns"
		warn "named above will ship inside the artifact and reach no response."
	else
		die "the staged artifact's schema does not match what this API models.
       Nothing was flipped and the running service is untouched; the release
       directory is at $TARGET.

       This is the check working. Add the column to Event in database.go and to
       the fixture in tests/main_test.go, let TestEventStructCoversEveryColumn
       tell you what else to fix, ship the binary, then publish again.

       If you have decided to publish the data first and update the API after,
       re-run with --allow-schema-drift."
	fi
fi

# ---------------------------------------------------------------------------
# 6. Flip and restart
# ---------------------------------------------------------------------------

step "Flipping the symlink and restarting"

# Every remote read from here to the end of the script is written as
# `if ! VAR=$(remote …)`. A bare `VAR=$(remote …)` takes the exit status of the
# command substitution, so under `set -e` an unreachable box stops the script
# dead between the flip and any rollback — no message, no rollback, exit 1, and
# a release left live that nobody checked. Inside an `if` condition the
# assignment is exempt from `set -e` and the failure can be answered.
#
# There is deliberately no `trap … ERR` doing this centrally. It would not fire
# for any of these sites, because ERR traps do not fire inside `if` conditions
# or `&&`/`||` chains, which is where every one of them now lives; it would add
# a second, invisible path into rollback, which is the most destructive thing
# this script does; and it would need its own guard against rolling back twice
# when a `die` path has already done it. Explicit at each site is longer and
# reviewable.
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
if ! RESTARTS_BEFORE=$(remote "systemctl show $SERVICE -p NRestarts --value 2>/dev/null || echo 0"); then
	# The symlink has moved but nothing has restarted, so the running process
	# still holds the previous inode and callers are unaffected. Putting the
	# symlink back is cheap and leaves the box consistent. Carrying on with a
	# guessed baseline is not: a wrong NRestarts turns the poll below into a
	# false CRASHLOOP verdict, which rolls back anyway — with a worse story.
	warn "could not read NRestarts from the box"
	rollback
	die "lost the box immediately after the symlink flip; the flip was undone.
       Nothing had restarted, so nothing was serving the new release yet."
fi

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

if ! HEALTH=$(remote "curl -s --max-time 10 $HEALTH_URL"); then
	# Unlike the search probe below, this one does roll back. /health answered
	# seconds ago — the wait loop above will not leave this block otherwise — so
	# a failure now is the service going away after coming up, not a checker
	# that was never able to run. curl's own failure is included in that: a
	# refused connection here means nothing is listening.
	warn "the service stopped answering $HEALTH_URL after reporting healthy"
	rollback
	die "the new release came up and then stopped answering. It was rolled back;
       the rejected release was left at $TARGET for inspection."
fi

# Parsed locally with python3 — already a hard dependency for validate.py —
# rather than by piping into a remote jq per field. One round trip, one place
# where a mismatch is decided, and no shell-quoting a jq program through ssh.
#
# Asserts, in one pass: status is ok; both languages resolve to the release
# just published (this is the check that catches a missed restart); the hashes
# the process reports match the checksums that shipped with the artifact; every
# row is indexed in both languages; the category vocabulary is non-empty; and
# the landmark flag is present.
#
# The category assertion is here because nothing else can make it. An artifact
# whose `category` column carries nothing on any row has the right schema, so
# the schema guard passes it; validate.py's invariant 13 is what should have
# caught it upstream, and if that has failed there is no second opinion. The API
# rejects every ?category= against such an artifact — correctly, since nothing
# can match — which means publishing one breaks every client's category filter
# in a way that only shows up as 400s in someone else's logs. The service says
# so once in its boot log, which no release reads. /health carries it so this
# does.
#
# Lines prefixed with ! are warnings rather than failures; see the dispatch
# below.
if ! VERIFY_OUT=$(python3 - "$TARGET" "$SOURCE_DIR/SHA256SUMS" "$HEALTH" "$ALLOW_SCHEMA_DRIFT" <<'PY'
import json, sys, pathlib

target, sums_path = sys.argv[1], sys.argv[2]
health = json.loads(sys.argv[3])
allow_schema_drift = sys.argv[4] == "1"

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

    # Absent, rather than empty, when the running binary predates the field.
    # That is not a reason to fail a publish and roll back: the binary and the
    # artifact are released by two different scripts on purpose, so a data
    # release against an older binary is an ordinary state of the world. It is
    # said out loud so that a silently skipped check cannot read as a passing
    # one.
    cats = db.get("categories")
    if cats is None:
        notes.append(f"!{lang}: /health carries no category vocabulary; the running binary "
                     f"predates the field, so this release is not checked for one")
    elif not cats.get("present"):
        # No column at all: the artifact is older than 2026-08-09. The schema
        # guard should already have refused it, so reaching here means it was
        # bypassed, and --allow-schema-drift is exactly that bypass.
        msg = (f"{lang}: the published artifact has no `category` column, so ?category= is "
               f"rejected for every value and /api/categories is empty")
        (notes.append("!" + msg) if allow_schema_drift else problems.append(msg))
    elif not cats.get("count"):
        # The column is there and every row is NULL or blank. No schema check
        # can see this, and --allow-schema-drift does not cover it: that flag is
        # for data the API cannot yet express, not for data that is not there.
        problems.append(
            f"{lang}: the `category` column carries no values on any of {db['rows']} rows.\n"
            f"         validate.py invariant 13 says every row has one, so this artifact\n"
            f"         should not exist. Every ?category= will be rejected until it is fixed.")
    else:
        notes.append(f'{lang}: {cats["count"]} categories')

    # The same three states as categories, and the same reason for reporting
    # them here: nothing else in this script can see any of them.
    #
    # The first case is the one only this can report. The schema guard runs the
    # API's test from the LOCAL repo — it proves the staged artifact matches the
    # checkout on this laptop, and says nothing about how old the binary on the
    # box is — so publishing a landmark column against a binary that predates
    # the field passes every other check green. That is not an incident: the
    # column simply reaches no response until the binary catches up, and nothing
    # consuming this API reads it yet. It is worth saying out loud anyway,
    # because the symptom is a field that is quietly missing rather than one
    # that is wrong, and those are the ones that go unnoticed.
    #
    # Where it departs from categories: an empty count is a WARNING, not a
    # failure. validate.py invariant 14 pins every value to 0 or 1 and
    # deliberately sets no target fraction — the flag is an editorial
    # judgement — so an artifact with no landmarks is legal data in a way an
    # artifact with no categories is not. It is still said out loud, because it
    # empties the one UI control the column exists to drive.
    lm = db.get("landmark")
    if lm is None:
        notes.append(f"!{lang}: /health carries no landmark flag; the running binary predates "
                     f"the field, so this release is not checked for one. If this release is "
                     f"the one adding `landmark`, run publish-api.sh to catch the binary up — "
                     f"until then the column ships but reaches no response")
    elif not lm.get("present"):
        # No column at all: the artifact predates 2026-08-12. The schema guard
        # should already have refused it, so reaching here means it was
        # bypassed, and --allow-schema-drift is exactly that bypass.
        msg = (f"{lang}: the published artifact has no `landmark` column, so ?landmark= is "
               f"rejected for every value and every event reports landmark false")
        (notes.append("!" + msg) if allow_schema_drift else problems.append(msg))
    elif not lm.get("count"):
        notes.append(f"!{lang}: the `landmark` column carries the flag on none of {db['rows']} "
                     f"rows. That is legal — the validator sets no target fraction — but it "
                     f"empties the switch the flag exists for. Check it was intended.")
    else:
        notes.append(f'{lang}: {lm["count"]} landmarks')

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
printf '%s\n' "$VERIFY_OUT" | while IFS= read -r line; do
	case "$line" in
		"") continue ;;
		"!"*) warn "${line#!}" ;;
		*) ok "$line" ;;
	esac
done

# Vocabulary assertion. The strongest single end-to-end signal, and the only
# one here that would catch a tokenizer change: it goes through the real search
# endpoint rather than /health. Cyrillic is the case that actually breaks.
#
# Two failures live here and they are kept apart, because they deserve opposite
# answers. A term that matches nothing is an answer, and a damning one: the
# index or the tokenizer is broken for that script, and the release comes back
# out. A probe that never produced an answer at all — the box unreachable, jq
# gone, curl dead — is evidence about the box, not about the release, and
# reverting a publish that may be perfectly good because the checker broke would
# be the wrong trade. That one exits non-zero with the release left running and
# the commands to finish by hand.
#
# Both used to be the same flag, and before that both used to read as a pass:
# under `[ "$total" -eq 0 ]` an empty $total errored to false, the elif errored
# to false too, and control fell through to the ok branch — so a verify whose
# probe had died printed ok with an empty count.
step "Vocabulary"

if [ -z "$API_KEY" ]; then
	# Reported at preflight, before anything was staged. Repeated here so the
	# absence of the assertion is visible in the release output too.
	warn "no API key was readable at preflight; skipping the search assertion"
else
	vocab_failed=0
	probe_broken=0
	for pair in "ru:$VOCAB_RU_TERM:$VOCAB_RU_LAST" "en:$VOCAB_EN_TERM:$VOCAB_EN_LAST"; do
		lang=${pair%%:*}; rest=${pair#*:}
		term=${rest%:*}; last=${rest##*:}
		if ! total=$(remote "curl -s --max-time 10 -H 'X-API-KEY: $API_KEY' --get \
			--data-urlencode 'q=$term' --data-urlencode 'lang=$lang' \
			'$API_URL/search' | jq -r '.pagination.total // 0'"); then
			warn "$lang: could not reach the box to run the search probe"
			probe_broken=1
			continue
		fi
		case "$total" in
		'' | *[!0-9]*)
			# ssh worked and the answer is unusable: jq missing (preflight says
			# it was there, so it went away mid-publish), curl failing inside
			# the remote pipeline, or /search answering something that is not
			# JSON. Same bucket as an unreachable box — we never got an answer.
			warn "$lang: the search probe returned no usable total (got '$total')"
			probe_broken=1
			;;
		0)
			warn "$lang: '$term' matched NOTHING (was $last) — the index or the tokenizer is broken for this script"
			vocab_failed=1
			;;
		"$last")
			ok "$lang: '$term' -> $total"
			;;
		*)
			upper=$(printf '%s' "$lang" | tr '[:lower:]' '[:upper:]')
			ok "$lang: '$term' -> $total (was $last; update VOCAB_${upper}_LAST in this script if intended)"
			;;
		esac
	done

	# A proven-bad index outranks a broken probe: if one language answered zero,
	# roll back whatever happened to the other.
	if [ "$vocab_failed" -eq 1 ]; then
		rollback
		die "a language returned no search results at all — not publishing this"
	fi

	if [ "$probe_broken" -eq 1 ]; then
		warn "the release is LIVE and the search assertion never ran against it"
		die "could not run the search assertion — the release was NOT rolled back.
       Everything /health can prove about $RELEASE passed; only the end-to-end
       search check is missing, and rolling back a release on the strength of a
       broken checker would be worse than leaving it up unverified.

       Check it by hand once the box is reachable (the key is the first entry
       in /etc/bitcal/api.env):

         ssh $SSH_HOST \"curl -s -H 'X-API-KEY: <key>' --get \\
           --data-urlencode 'q=$VOCAB_RU_TERM' --data-urlencode 'lang=ru' \\
           '$API_URL/search' | jq .pagination.total\"

       Expected around $VOCAB_RU_LAST. If it answers 0, undo the release with:

         ssh $SSH_HOST \"ln -sfn '$PREVIOUS_RELEASE' $REMOTE_DATA/current.new \\
           && mv -T $REMOTE_DATA/current.new $REMOTE_DATA/current \\
           && systemctl restart $SERVICE\""
	fi
fi

# ---------------------------------------------------------------------------
# 8. Prune
# ---------------------------------------------------------------------------

step "Pruning old releases (keeping $KEEP)"

# Housekeeping, after a release that has already passed every assertion. A
# failure here is not grounds to touch the release: old directories left on
# disk are a disk-space problem, not a serving problem. It is still said out
# loud, because the whole point of pruning is that nobody watches disk usage.
PRUNED=""
if ! PRUNED=$(remote "bash -s" -- "$REMOTE_DATA" "$KEEP" <<'REMOTE'
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
); then
	warn "pruning failed; old releases are still on the box and will need clearing by hand:"
	warn "  ssh $SSH_HOST 'ls -1 $REMOTE_DATA/releases'"
	warn "the release itself is published and verified — this does not affect what is served"
	PRUNED=""
elif [ -n "$PRUNED" ]; then
	printf '%s\n' "$PRUNED" | sed 's/^/       removed /'
else
	ok "nothing to prune"
fi

# Cosmetic, and the last thing this script does. A box that becomes unreachable
# between the prune and here has not affected a release that is already live and
# already verified, so this reports what it knows rather than exiting on it.
if ! REMAINING=$(remote "ls -1 $REMOTE_DATA/releases | wc -l | tr -d ' '"); then
	REMAINING="an unknown number of"
fi

# ---------------------------------------------------------------------------

step "Published $RELEASE"
printf '    %s releases retained, serving %s\n' "$REMAINING" "$RELEASE"
printf '    verify by hand: ssh %s \"curl -s %s | jq .\"\n' "$SSH_HOST" "$HEALTH_URL"
