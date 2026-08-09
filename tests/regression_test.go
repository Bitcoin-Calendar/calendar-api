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
