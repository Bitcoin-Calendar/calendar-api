#!/usr/bin/env bash
#
# publish-api.sh — ship the bitcal-api binary to the box.
#
# The counterpart to publish-db.sh, and deliberately its mirror image: that
# script ships data and never rebuilds anything, this one ships a binary and
# never touches the data. Between them they are the only two ways the box
# changes, and keeping them separate is what makes each one reviewable.
#
#   local  preflight  clean tree, HEAD pushed, make test, version string
#   remote preflight  toolchain, and a /health snapshot to roll back to
#   remote build      git archive HEAD -> temp dir on the box, build, make test
#   remote install    back up the running binary, then rename the new one over it
#   remote restart    systemctl restart (mandatory — the binary is running)
#   remote verify     /health: the version moved and the DATA DID NOT
#   remote prune      keep the last N .bak binaries
#
# Any failure after the install rolls back to the previous binary automatically.
#
# This is a transcription, not an invention. The procedure below was walked by
# hand twice on 2026-08-09 — bitcal-api.bak-20260809T160545Z and the binary that
# replaced it are both still on the box — and the steps are in this order
# because that is the order that worked.
#
# Why the build happens ON THE BOX
# --------------------------------
# mattn/go-sqlite3 needs CGO, and CGO ties the binary to the C library it was
# built against. The box is Ubuntu 24.04 / glibc 2.39; a binary built on this
# Mac will not start there at all, and one built on Alpine (musl) will not
# either. Building on the target makes the match true by construction rather
# than by a Makefile target someone has to remember to use. `make build-ubuntu`
# does the same job in Docker and is the documented alternative, but it needs a
# Docker daemon running locally, which is one more thing to be wrong on the day
# something is urgent. Go 1.23.12 and gcc are already installed on the box —
# they were installed 92 seconds before the first release was built there.
#
# The source is exported with `git archive` into a temp directory and removed
# afterwards, so no source tree and no build output persist on a production
# host. That also means the tree that gets built is exactly what is committed:
# an rsync of the working directory would quietly ship uncommitted edits under
# a version string naming a commit that does not contain them.
#
# Why VERSION is passed explicitly
# --------------------------------
# The Makefile derives VERSION from `git rev-parse --short HEAD`. The exported
# tree has no .git, so on the box that fallback yields `0.1.0-unknown` — and
# /health reporting `unknown` is how you find out afterwards. It is passed on
# the command line here for that reason; do not "simplify" it away.
#
# Usage:
#   ./publish-api.sh                   # build, install, verify, with a prompt
#   ./publish-api.sh --dry-run         # every local and remote check, no install
#   ./publish-api.sh --yes             # no prompt
#   ./publish-api.sh --keep 5          # how many .bak binaries to retain
#   ./publish-api.sh --version 0.2.0   # override the derived version string
#   ./publish-api.sh --allow-dirty     # build a tree git cannot account for
#   ./publish-api.sh --host root@example
#
set -euo pipefail

# --- defaults ---------------------------------------------------------------

SSH_HOST="${BITCAL_HOST:-root@169.58.80.219}"
REMOTE_API="/srv/bitcal/api"
BINARY="bitcal-api"
SERVICE="bitcal-api"
HEALTH_URL="http://127.0.0.1:3000/health"
# Not on root's PATH: the toolchain is installed under /usr/local/go and a bare
# `which go` on the box finds nothing.
REMOTE_GO="/usr/local/go/bin"
KEEP=5
DRY_RUN=0
ASSUME_YES=0
ALLOW_DIRTY=0
VERSION=""

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# --- argument parsing -------------------------------------------------------

while [ $# -gt 0 ]; do
	case "$1" in
	--host) SSH_HOST="$2"; shift 2 ;;
	--keep) KEEP="$2"; shift 2 ;;
	--version) VERSION="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	--allow-dirty) ALLOW_DIRTY=1; shift ;;
	-h | --help) sed -n '2,58p' "$0"; exit 0 ;;
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

# One SSH connection, reused — same reason as publish-db.sh: without
# multiplexing the health poll pays a full handshake per attempt.
SSH_CTL_DIR=$(mktemp -d "${TMPDIR:-/tmp}/bitcal-api-ssh.XXXXXX")
SSH_OPTS=(-o BatchMode=yes -o ControlMaster=auto -o ControlPath="$SSH_CTL_DIR/ctl" -o ControlPersist=180)

remote() { ssh "${SSH_OPTS[@]}" "$SSH_HOST" "$@"; }

REMOTE_BUILD=""
close_ssh() {
	if [ -n "$REMOTE_BUILD" ]; then
		remote "rm -rf '$REMOTE_BUILD'" >/dev/null 2>&1 || true
	fi
	ssh "${SSH_OPTS[@]}" -O exit "$SSH_HOST" >/dev/null 2>&1 || true
	rm -rf "$SSH_CTL_DIR"
}
trap close_ssh EXIT

# ---------------------------------------------------------------------------
# 1. Local preflight
# ---------------------------------------------------------------------------

step "Local preflight"

cd "$REPO_ROOT"
git rev-parse --git-dir >/dev/null 2>&1 || die "$REPO_ROOT is not a git repository"

# Unlike publish-db.sh, this check is not scoped to a subdirectory: this repo is
# the artifact. Anything uncommitted is something the built binary will not
# contain, under a version string that names a commit which does not contain it
# either.
DIRTY=$(git status --porcelain)
HEAD_SHA=$(git rev-parse --short HEAD)

if [ -n "$DIRTY" ]; then
	if [ "$ALLOW_DIRTY" -eq 1 ]; then
		warn "the working tree is dirty and --allow-dirty was given:"
		printf '%s\n' "$DIRTY" | sed 's/^/         /'
		warn "the build exports HEAD, so these changes will NOT be in the binary"
	else
		printf '%s\n' "$DIRTY" | sed 's/^/       /'
		die "the working tree is dirty.
       The build exports HEAD with git archive, so these changes would not be in
       the binary — but /health would report a version string naming this commit
       as though they were. Commit them, or re-run with --allow-dirty."
	fi
fi

# The version string is only useful if it identifies something another person
# can check out. A binary built from an unpushed commit is one force-push away
# from being unreproducible.
if git rev-parse --verify origin/main >/dev/null 2>&1; then
	if git merge-base --is-ancestor HEAD origin/main; then
		ok "HEAD $HEAD_SHA is on origin/main"
	elif [ "$ALLOW_DIRTY" -eq 1 ]; then
		warn "HEAD $HEAD_SHA is not on origin/main; the version string will name a commit nobody else has"
	else
		die "HEAD ($HEAD_SHA) is not an ancestor of origin/main.
       The binary would report a version string for a commit that exists only on
       this machine. Push it first, or re-run with --allow-dirty."
	fi
else
	warn "no origin/main to compare against; cannot confirm the commit is pushed"
fi

[ -n "$VERSION" ] || VERSION="0.1.0-$HEAD_SHA"
ok "version: $VERSION"

# The suite is black box: it builds the binary, stages a fixture at 0444 in a
# 0555 directory and drives it over HTTP. Running it here is not redundant with
# running it on the box — this catches a broken change before anything is
# copied anywhere, and costs about six seconds.
command -v go >/dev/null 2>&1 || die "go not found on PATH; cannot run the preflight test"
if ! TEST_OUT=$(make test 2>&1); then
	printf '%s\n' "$TEST_OUT" | tail -30 | sed 's/^/       /'
	die "make test failed locally; nothing was built or copied"
fi
ok "make test: PASS locally"

# ---------------------------------------------------------------------------
# 2. Remote preflight — capture what is live now, so a rollback has a target
# ---------------------------------------------------------------------------

step "Remote preflight ($SSH_HOST)"

remote true 2>/dev/null || die "cannot reach $SSH_HOST over ssh"
ok "ssh reachable"

remote "systemctl cat $SERVICE >/dev/null 2>&1" \
	|| die "$SERVICE is not installed on $SSH_HOST"

remote "test -x '$REMOTE_API/$BINARY'" \
	|| die "no binary at $REMOTE_API/$BINARY — this script replaces an existing install, it does not bootstrap one"

remote "test -x '$REMOTE_GO/go'" \
	|| die "no Go toolchain at $REMOTE_GO/go on the box.
       The build happens there on purpose (CGO/glibc); see the header. Install
       Go, or build locally with 'make build-ubuntu' and install by hand."
REMOTE_GO_VERSION=$(remote "$REMOTE_GO/go version")
ok "toolchain: $REMOTE_GO_VERSION"

# The baseline. Everything after the install is judged against this, and the
# data half of it is the assertion that matters most: a binary deploy must not
# move the databases. If it does, something restarted into a different release
# and the two deploy paths have collided.
BEFORE_HEALTH=$(remote "curl -sf --max-time 10 $HEALTH_URL") \
	|| die "the service is not healthy before we start; fix that first — a rollback needs somewhere to roll back to"

BEFORE_VERSION=$(printf '%s' "$BEFORE_HEALTH" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","?"))')
ok "currently running: $BEFORE_VERSION"
python3 - "$BEFORE_HEALTH" <<'PY'
import json, sys
h = json.loads(sys.argv[1])
for lang, db in sorted(h.get("databases", {}).items()):
    release = db["path"].rsplit("/", 2)[-2]
    print(f'           {lang}: {db["rows"]} rows  {db["sha256"][:16]}…  {release}')
PY

if [ "$BEFORE_VERSION" = "$VERSION" ]; then
	warn "the box is already running $VERSION — this will rebuild and reinstall the same version string"
fi

# ---------------------------------------------------------------------------
# 3. Confirm
# ---------------------------------------------------------------------------

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="$REMOTE_API/$BINARY.bak-$STAMP"

step "About to publish the binary"
printf '    version      %s  (was %s)\n' "$VERSION" "$BEFORE_VERSION"
printf '    commit       %s\n' "$(git log -1 --format='%h %s' HEAD)"
printf '    host         %s\n' "$SSH_HOST"
printf '    build        on the box, %s\n' "$REMOTE_GO_VERSION"
printf '    installs to  %s/%s\n' "$REMOTE_API" "$BINARY"
printf '    backup       %s\n' "$BACKUP"

if [ "$DRY_RUN" -eq 1 ]; then
	step "Dry run — every check above passed; nothing was built, copied or restarted"
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
# 4. Build on the box
# ---------------------------------------------------------------------------
# Under /tmp and removed on exit, so a production host does not accumulate
# source trees. Nothing here touches $REMOTE_API until the build and the test
# have both passed.

step "Building $VERSION on the box"

REMOTE_BUILD=$(remote "mktemp -d /tmp/bitcal-build.XXXXXX")

# git archive rather than rsync: exactly the committed tree, no .git, no build
# artefacts, no editor droppings.
git archive --format=tar HEAD | remote "tar -x -C '$REMOTE_BUILD'" \
	|| die "could not export the source tree to the box"
ok "exported HEAD to $REMOTE_BUILD"

if ! BUILD_OUT=$(remote "cd '$REMOTE_BUILD' && PATH=$REMOTE_GO:\$PATH make build VERSION='$VERSION' 2>&1"); then
	printf '%s\n' "$BUILD_OUT" | tail -30 | sed 's/^/       /'
	die "the build failed on the box; nothing was installed"
fi
ok "built $BINARY"

# The same suite again, on the target. This is the layer that proves the build
# tag, the read-only open and the boot probe all work against the box's own
# libc and kernel — none of which the local run can speak for.
if ! REMOTE_TEST_OUT=$(remote "cd '$REMOTE_BUILD' && PATH=$REMOTE_GO:\$PATH make test 2>&1"); then
	printf '%s\n' "$REMOTE_TEST_OUT" | tail -30 | sed 's/^/       /'
	die "make test failed ON THE BOX; nothing was installed.
       A local pass and a remote failure means the difference is the platform —
       look at CGO, the fts5 tag, and the glibc version before anything else."
fi
ok "make test: PASS on the box"

# Prove the thing that was just built actually reports the version we asked
# for, before it replaces a working binary. This is the check that catches the
# `0.1.0-unknown` failure: the exported tree has no .git, so a VERSION that
# failed to reach the Makefile shows up here rather than in /health afterwards.
BUILT_VERSION=$(remote "'$REMOTE_BUILD/$BINARY' --version 2>/dev/null || true")
if [ -z "$BUILT_VERSION" ]; then
	# The binary has no --version flag today; fall back to the string table.
	BUILT_VERSION=$(remote "strings '$REMOTE_BUILD/$BINARY' | grep -Fx '$VERSION' | head -1 || true")
fi
if [ -n "$BUILT_VERSION" ]; then
	ok "the built binary carries $VERSION"
else
	warn "could not confirm the version string inside the binary before install"
	warn "  /health after the restart is the authoritative check — it is asserted below"
fi

# ---------------------------------------------------------------------------
# 5. Install
# ---------------------------------------------------------------------------
# Back up first, then rename over the target. The rename matters: writing over
# a running executable in place fails with ETXTBSY, while a rename replaces the
# directory entry and lets the running process keep its inode until it exits.

step "Installing"

remote "cp -p '$REMOTE_API/$BINARY' '$BACKUP'" \
	|| die "could not back up the running binary; refusing to install without a rollback target"
ok "backed up the running binary to $(basename "$BACKUP")"

remote "cp '$REMOTE_BUILD/$BINARY' '$REMOTE_API/$BINARY.new' \
	&& chown root:root '$REMOTE_API/$BINARY.new' \
	&& chmod 0755 '$REMOTE_API/$BINARY.new' \
	&& mv -f '$REMOTE_API/$BINARY.new' '$REMOTE_API/$BINARY'" \
	|| die "could not install the new binary; the running one is untouched and still in place"
ok "installed $REMOTE_API/$BINARY (root:root 0755)"

# ---------------------------------------------------------------------------
# 6. Restart
# ---------------------------------------------------------------------------

rollback() {
	printf '\n%s==> Rolling back to %s%s\n' "$BOLD" "$BEFORE_VERSION" "$OFF"
	remote "cp -p '$BACKUP' '$REMOTE_API/$BINARY.rollback' \
		&& chown root:root '$REMOTE_API/$BINARY.rollback' \
		&& chmod 0755 '$REMOTE_API/$BINARY.rollback' \
		&& mv -f '$REMOTE_API/$BINARY.rollback' '$REMOTE_API/$BINARY' \
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
		NOW=$(remote "curl -sf --max-time 5 $HEALTH_URL" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","?"))' 2>/dev/null || echo "?")
		ok "rolled back; $NOW is serving again"
	else
		warn "ROLLBACK DID NOT COME UP — the service needs attention now"
		warn "  ssh $SSH_HOST 'journalctl -u $SERVICE -n 50 --no-pager'"
		remote "journalctl -u $SERVICE -n 30 --no-pager" 2>/dev/null | sed 's/^/       /' || true
	fi
}

step "Restarting"

# Same NRestarts trick as publish-db.sh: a manual restart does not increment it,
# so any increase afterwards means systemd is restarting a process that keeps
# dying, and the failure is reported in seconds rather than at the timeout.
RESTARTS_BEFORE=$(remote "systemctl show $SERVICE -p NRestarts --value 2>/dev/null || echo 0")

remote "systemctl restart $SERVICE" || { rollback; die "restart failed"; }

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
	CRASHLOOP) warn "the service is crash-looping on the new binary (${ELAPSED}s)" ;;
	*) warn "the service did not answer /health within ${ELAPSED}s" ;;
	esac
	remote "journalctl -u $SERVICE -n 25 --no-pager" 2>/dev/null | sed 's/^/       /' || true
	rollback
	die "publish failed: the new binary did not come up."
fi
ok "healthy after ${ELAPSED}s"

# ---------------------------------------------------------------------------
# 7. Verify — the version moved and the data did not
# ---------------------------------------------------------------------------
# Two assertions, and the second is the one worth writing down. publish-db.sh
# proves a data release changed the data; this proves a binary release did not.
# The two paths are independent and they have already drifted once — on
# 2026-08-09 the box served freshly published artifacts for about 35 minutes
# while still running the previous binary. A binary deploy that quietly moved
# the databases would be indistinguishable from a correct one, from the outside,
# until someone compared hashes. So compare them here, every time.

step "Verifying"

AFTER_HEALTH=$(remote "curl -s --max-time 10 $HEALTH_URL")

if ! VERIFY_OUT=$(python3 - "$VERSION" "$BEFORE_HEALTH" "$AFTER_HEALTH" <<'PY'
import json, sys

want_version, before, after = sys.argv[1], json.loads(sys.argv[2]), json.loads(sys.argv[3])
problems, notes = [], []

if after.get("status") != "ok":
    problems.append(f'status is {after.get("status")!r}, want "ok"')

got = after.get("version")
if got != want_version:
    problems.append(f'version is {got!r}, want {want_version!r} — the restart did not pick up the new binary')
else:
    notes.append(f'version {before.get("version")!r} -> {got!r}')

# The data must be byte-for-byte the file that was already being served.
b_dbs, a_dbs = before.get("databases", {}), after.get("databases", {})
if set(b_dbs) != set(a_dbs):
    problems.append(f'the set of databases changed: {sorted(b_dbs)} -> {sorted(a_dbs)}')

for lang in sorted(set(b_dbs) & set(a_dbs)):
    b, a = b_dbs[lang], a_dbs[lang]
    moved = []
    if b["sha256"] != a["sha256"]:
        moved.append(f'sha256 {b["sha256"][:16]}… -> {a["sha256"][:16]}…')
    if b["rows"] != a["rows"]:
        moved.append(f'rows {b["rows"]} -> {a["rows"]}')
    if b["path"] != a["path"]:
        moved.append(f'path {b["path"]} -> {a["path"]}')
    if moved:
        problems.append(
            f'{lang}: a binary deploy moved the data — ' + '; '.join(moved) + '\n'
            f'         Nothing here publishes databases. Either publish-db.sh ran\n'
            f'         concurrently, or the current symlink changed underneath us.')
    else:
        notes.append(f'{lang}: {a["rows"]} rows, sha256 unchanged, same release directory')

    fts = a.get("fts", {})
    if not fts.get("consistent") or fts.get("indexed") != a["rows"]:
        problems.append(f'{lang}: index covers {fts.get("indexed")} of {a["rows"]} rows')

print("\n".join(notes))
if problems:
    print("PROBLEMS", file=sys.stderr)
    print("\n".join("  " + p for p in problems), file=sys.stderr)
    sys.exit(1)
PY
); then
	printf '%s\n' "$VERIFY_OUT" | sed 's/^/       /'
	printf '%s\n' "$AFTER_HEALTH" | sed 's/^/       /'
	rollback
	die "/health does not describe the binary that was just installed, or the data moved."
fi
printf '%s\n' "$VERIFY_OUT" | while IFS= read -r line; do [ -n "$line" ] && ok "$line"; done

# One real request through the real router, because /health does not exercise
# the database read path or the FTS module. A binary built without -tags fts5
# cannot exist (fts5_required.go refuses to compile), so this is defence in
# depth rather than a gap — but it is the assertion that would catch a driver
# or artifact problem that only appears under a query.
API_KEY=$(remote "sed -n 's/^API_KEYS=//p' /etc/bitcal/api.env | cut -d, -f1")
if [ -n "$API_KEY" ]; then
	SEARCH_TOTAL=$(remote "curl -s --max-time 10 -H 'X-API-KEY: $API_KEY' --get \
		--data-urlencode 'q=биткоин' --data-urlencode 'lang=ru' \
		'http://127.0.0.1:3000/api/search' | jq -r '.pagination.total // 0'")
	if [ "${SEARCH_TOTAL:-0}" -gt 0 ]; then
		ok "search works through the new binary (ru 'биткоин' -> $SEARCH_TOTAL)"
	else
		warn "search returned nothing through the new binary — check the fts5 build tag"
		remote "journalctl -u $SERVICE -n 25 --no-pager" 2>/dev/null | sed 's/^/       /' || true
		rollback
		die "the new binary cannot serve search; rolled back."
	fi
else
	warn "could not read an API key from /etc/bitcal/api.env; skipping the search assertion"
fi

# ---------------------------------------------------------------------------
# 8. Prune
# ---------------------------------------------------------------------------

step "Pruning old binaries (keeping $KEEP)"

PRUNED=$(remote "bash -s" -- "$REMOTE_API" "$BINARY" "$KEEP" <<'REMOTE'
set -euo pipefail
dir="$1"; binary="$2"; keep="$3"
cd "$dir" || exit 0
# Names end in a UTC timestamp, so lexical order is chronological.
mapfile -t all < <(find . -maxdepth 1 -mindepth 1 -name "$binary.bak-*" -printf '%f\n' | sort)
count=${#all[@]}
[ "$count" -le "$keep" ] && exit 0
for f in "${all[@]:0:$((count - keep))}"; do
	rm -f "$dir/$f" && echo "$f"
done
REMOTE
)
if [ -n "$PRUNED" ]; then
	printf '%s\n' "$PRUNED" | sed 's/^/       removed /'
else
	ok "nothing to prune"
fi

REMAINING=$(remote "ls -1 $REMOTE_API/$BINARY.bak-* 2>/dev/null | wc -l | tr -d ' '")

# ---------------------------------------------------------------------------

step "Published $VERSION"
printf '    %s previous binaries retained, rollback target %s\n' "$REMAINING" "$(basename "$BACKUP")"
printf '    roll back by hand: ssh %s \"cp -p %s %s/%s && systemctl restart %s\"\n' \
	"$SSH_HOST" "$BACKUP" "$REMOTE_API" "$BINARY" "$SERVICE"
printf '    verify by hand:    ssh %s \"curl -s %s | jq .\"\n' "$SSH_HOST" "$HEALTH_URL"
