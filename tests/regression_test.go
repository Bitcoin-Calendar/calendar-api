package tests

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// commentAfterSemicolon matches a `--` comment sitting after a semicolon.
var commentAfterSemicolon = regexp.MustCompile(`;[^\n]*--`)

// TestNoCommentAfterFinalSemicolon is a static check on the source, because
// this mistake is invisible in review and produces no error at runtime — just
// a request that never returns. /api/tags shipped this way.
//
// SQLite prepares whatever follows a statement's semicolon as another
// statement, and a fragment holding only a comment prepares successfully to a
// NULL statement handle. go-sqlite3 steps that handle, gets back neither
// SQLITE_DONE nor SQLITE_ROW, calls sqlite3_reset and returns nil rather than
// io.EOF (sqlite3.go:2238) — so database/sql believes a row is waiting, asks
// for the next one, and loops forever.
func TestNoCommentAfterFinalSemicolon(t *testing.T) {
	sources, err := filepath.Glob("../*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("found no source files to scan")
	}

	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if commentAfterSemicolon.MatchString(line) {
				t.Errorf("%s:%d: a SQL comment after a semicolon hangs the query "+
					"forever rather than erroring:\n\t%s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// goSort matches the argument of a GORM .Order(...) call.
var goSort = regexp.MustCompile(`\.Order\(([^)]+)\)`)

// packageConst matches a package-level string constant, declared on its own or
// inside a const block, so a sort named by identifier — .Order(eventOrder) —
// can be resolved to the text it stands for.
var packageConst = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w+)\s*=\s*"([^"]*)"`)

// rawString matches a backtick-quoted Go string, which is how the raw SQL in
// this service is written.
var rawString = regexp.MustCompile("(?s)`([^`]*)`")

// sqlSort pulls the sort list out of an ORDER BY, stopping at LIMIT or the
// statement's end.
var sqlSort = regexp.MustCompile(`(?is)ORDER\s+BY\s+(.*?)(?:\s+LIMIT\b|;|$)`)

// TestPaginatedSortsBreakTies is a static check, for the same reason
// TestNoCommentAfterFinalSemicolon is one: the mistake it guards is invisible
// both in review and at runtime.
//
// SQL promises no order among rows an ORDER BY cannot separate, and it does not
// promise to make the same arbitrary choice twice. Sorted on the date alone —
// and 19 dates carry more than one event in the English artifact — two events
// on the same day may be returned in either order, so a caller walking pages
// can be handed one twice and never shown the other. Downstream that reads as a
// bot posting a duplicate and silently skipping a real event.
//
// A black-box test cannot catch this. The order today is stable because of the
// query plan SQLite happens to choose, not because anything requires it: with
// the tiebreaker removed, TestListOrderBreaksTiesById still passes. A new
// index, a driver upgrade or a different SQLite build can change that plan
// without a single line of this service changing. So the guarantee has to be
// asserted where it is actually made — in the sort itself.
//
// Only paginated sorts are checked. Without LIMIT/OFFSET there are no page
// boundaries for an unstable order to fall across, which is why /api/tags
// sorting by its GROUP BY key alone is fine and is not flagged here.
func TestPaginatedSortsBreakTies(t *testing.T) {
	// Every source file, not a named one: the handlers live in per-feature
	// files, and a sort added to a new file must not be born unchecked.
	paths, err := filepath.Glob("../*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sources []string
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		sources = append(sources, string(src))
	}
	if len(sources) == 0 {
		t.Fatal("found no source files to scan")
	}

	// Resolve string constants across the whole package first, so a sort named
	// by an identifier declared in another file — .Order(eventOrder) — can be
	// read wherever it is used.
	consts := map[string]string{}
	for _, source := range sources {
		for _, m := range packageConst.FindAllStringSubmatch(source, -1) {
			consts[m[1]] = m[2]
		}
	}

	checked := 0
	requireTiebreak := func(what, sort string) {
		t.Helper()
		checked++
		if !strings.Contains(strings.ToLower(sort), "id") {
			t.Errorf("%s sorts by %q with no tiebreaker: rows the sort cannot "+
				"separate come back in an order SQL does not guarantee, so paging "+
				"can repeat one event and drop another. Add a unique column — id.",
				what, sort)
		}
	}

	for _, source := range sources {
		// GORM sorts, whether written inline or by way of a constant.
		for _, m := range goSort.FindAllStringSubmatch(source, -1) {
			arg := strings.TrimSpace(m[1])
			sort, ok := consts[arg]
			if !ok {
				sort = strings.Trim(arg, `"`)
			}
			requireTiebreak(".Order("+arg+")", sort)
		}

		// Raw SQL sorts, but only where the statement also paginates.
		for _, m := range rawString.FindAllStringSubmatch(source, -1) {
			stmt := m[1]
			if !strings.Contains(strings.ToUpper(stmt), "OFFSET") {
				continue
			}
			for _, s := range sqlSort.FindAllStringSubmatch(stmt, -1) {
				// Strip SQL comments: the sort list in this service is annotated,
				// and a comment mentioning "id" would satisfy the check on its own.
				sort := ""
				for _, line := range strings.Split(s[1], "\n") {
					if i := strings.Index(line, "--"); i >= 0 {
						line = line[:i]
					}
					sort += " " + line
				}
				requireTiebreak("a paginated raw query", strings.TrimSpace(sort))
			}
		}
	}

	// Without this the test passes whenever the regexes stop matching — a
	// refactor renaming .Order or reformatting the SQL would silently retire the
	// guard rather than fail it.
	if checked < 3 {
		t.Errorf("found only %d paginated sorts to check, expected at least 3 "+
			"(/api/events, /api/events/tags/:tag, /api/search); the patterns this "+
			"test matches on no longer fit the source", checked)
	}
}

// TestContextDeadlineBreaksTheRunawayLoop proves the mechanism the service's
// request timeout depends on. The static check above catches the shape of the
// mistake; this shows that even when something does run away, a deadline on
// the query stops it.
//
// It runs the pathological statement deliberately, so if a future driver
// version changes this behaviour the test says so rather than the service
// hanging in production.
func TestContextDeadlineBreaksTheRunawayLoop(t *testing.T) {
	dbPath := filepath.Join(artifactDir, "events_ru.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_query_only=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// The exact shape that hung: a comment after the final semicolon.
	const pathological = `SELECT id FROM events ORDER BY id; -- a trailing comment`

	// How many rows the statement can honestly produce. Anything past this is
	// the driver looping.
	var realRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&realRows); err != nil {
		t.Fatalf("counting rows: %v", err)
	}

	t.Run("without a deadline it runs away", func(t *testing.T) {
		const cap = 100_000 // far past any honest answer, cheap to reach

		rows, err := db.Query(pathological)
		if err != nil {
			t.Skipf("driver now rejects the statement outright (%v); nothing to run away", err)
		}
		defer rows.Close()

		seen := 0
		for rows.Next() {
			seen++
			if seen >= cap {
				break
			}
		}

		switch {
		case seen >= cap:
			// Expected today: Next() keeps reporting rows that do not exist.
		case seen <= realRows:
			t.Logf("driver returned %d rows for %d real rows — it no longer loops "+
				"on a trailing comment. Good news, but leave the source guard in "+
				"place until the fix is identified.", seen, realRows)
		default:
			t.Errorf("driver produced %d rows for %d real rows: neither the known "+
				"runaway nor a correct result", seen, realRows)
		}
	})

	t.Run("with a deadline it returns", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			rows, err := db.QueryContext(ctx, pathological)
			if err != nil {
				done <- err
				return
			}
			defer rows.Close()
			for rows.Next() {
			}
			done <- rows.Err()
		}()

		select {
		case <-done:
			// Returned, which is the whole point. Whether it comes back as a
			// deadline error or as a clean end does not matter here.
		case <-time.After(15 * time.Second):
			t.Fatal("a context deadline did not stop the runaway query — the " +
				"service's request timeout cannot be relied on")
		}
	})
}
