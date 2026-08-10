package tests

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventStructCoversEveryColumn is the general guard, and it exists because
// the specific bug it generalises shipped invisibly with every check green:
// canonical gained a `category` column on 2026-08-09, the Event struct did not,
// and nothing in this suite had an opinion about the difference.
//
// It reads the artifact's own schema rather than listing the expected fields by
// hand, because a hand-written list has to be remembered — and remembering is
// the step that failed.
//
// It runs against /api/search as well as /api/events on purpose. The search
// handler enumerates its columns in a literal SELECT instead of letting GORM
// derive them from the struct, so a field can be present in the model, correct
// on every other endpoint, and still missing from search. A version of this
// test that checked one endpoint would go green on a half-fix.
//
// It asserts the value survived, not merely that the key is there, and that is
// load-bearing rather than thoroughness. Checking key presence alone cannot see
// the search half of this bug at all: Category is a plain string on a Go
// struct, so encoding/json writes `"category":""` whether the SELECT fetched
// the column or never mentioned it. The same holds for created_at and
// updated_at, which arrive as a JSON null either way. Presence proves the model
// has the field; only the value proves the statement went and got it. This was
// measured, not reasoned about — an earlier draft of this test checked presence
// only, and passed against a search handler that emitted an empty category for
// every row.
func TestEventStructCoversEveryColumn(t *testing.T) {
	// Columns deliberately not exposed. Empty today. Anything added here needs
	// a reason written beside it: the point of this test is that an omission
	// must be a decision someone recorded, not one nobody noticed.
	notExposed := map[string]string{}

	artifact := filepath.Join(artifactDir, "events_ru.db")
	columns := eventColumns(t, artifact)
	if len(columns) == 0 {
		t.Fatal("pragma table_info(events) returned no columns")
	}

	for _, ep := range []struct{ name, path string }{
		// A limit high enough to take every fixture row, because the columns
		// that are NULL on one row and set on another — media, references, the
		// timestamps — can only be proven on the row that has them.
		{"events", "/api/events?lang=ru&limit=100"},
		{"search", "/api/search?lang=ru&q=bitcoin&limit=100"},
	} {
		t.Run(ep.name, func(t *testing.T) {
			var body struct {
				Events []map[string]json.RawMessage `json:"events"`
			}
			if code := getAs(t, apiKey3, ep.path, &body); code != http.StatusOK {
				t.Fatalf("%s: want 200, got %d", ep.path, code)
			}
			// Without a row there is nothing to inspect, and the test would
			// pass by measuring nothing at all.
			if len(body.Events) == 0 {
				t.Fatalf("%s returned no events; this test cannot measure anything", ep.path)
			}

			// Which columns this endpoint proved it can carry a real value for.
			// Collected across every row rather than per row, since a column
			// that is legitimately NULL here may be set two rows down.
			proven := map[string]bool{}

			for _, got := range body.Events {
				id, ok := got["id"]
				if !ok {
					t.Fatalf("%s returned an event with no id; nothing can be looked up", ep.path)
				}
				stored := eventRow(t, artifact, string(id))

				for _, col := range columns {
					if _, skip := notExposed[col]; skip {
						continue
					}
					raw, present := got[col]
					if !present {
						continue // reported once, below
					}
					// A NULL in the artifact proves nothing either way: null is
					// the correct rendering, so this row cannot distinguish a
					// fetched column from an unfetched one.
					if stored[col] == nil {
						continue
					}
					if !isNullOrEmpty(raw) {
						proven[col] = true
					}
				}
			}

			for _, col := range columns {
				if why, ok := notExposed[col]; ok {
					t.Logf("column %q is deliberately not exposed: %s", col, why)
					continue
				}
				if _, present := body.Events[0][col]; !present {
					t.Errorf("the events table has a %q column and %s does not emit it at all.\n"+
						"Add the field to Event in database.go — and, for search, to the\n"+
						"explicit SELECT in ftsSearchHandler, which names its columns by\n"+
						"hand and will not pick it up otherwise.", col, ep.path)
					continue
				}
				if !proven[col] {
					t.Errorf("%s emits a %q key but never a value: every row that has one\n"+
						"stored came back null or empty. The field exists on the model, so\n"+
						"this is the statement, not the struct — add e.%s to the explicit\n"+
						"SELECT in ftsSearchHandler, which enumerates its columns by hand.",
						ep.path, col, col)
				}
			}
		})
	}
}

// eventRow reads one row of events by id, keyed by column name, so the test can
// ask what the artifact actually holds before deciding whether a null in the
// response is honest or lossy.
func eventRow(t *testing.T, path, id string) map[string]any {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT * FROM events WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("selecting event %s: %v", id, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns for event %s: %v", id, err)
	}
	if !rows.Next() {
		t.Fatalf("the response named event %s, which is not in the artifact", id)
	}

	cells := make([]any, len(cols))
	into := make([]any, len(cols))
	for i := range cells {
		into[i] = &cells[i]
	}
	if err := rows.Scan(into...); err != nil {
		t.Fatalf("scanning event %s: %v", id, err)
	}

	out := make(map[string]any, len(cols))
	for i, c := range cols {
		// A stored empty string is indistinguishable from an unfetched one in
		// the response, so treat it as nothing to prove rather than as a value
		// the endpoint failed to carry.
		if s, ok := cells[i].(string); ok && s == "" {
			continue
		}
		out[c] = cells[i]
	}
	return out
}

// isNullOrEmpty reports whether a JSON value carries nothing — the two shapes a
// dropped column takes, depending on whether its Go field is a pointer.
func isNullOrEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == `""`
}

// TestFixtureSchemaMatchesCanonical closes the loop that TestEventStructCovers-
// EveryColumn leaves open.
//
// That test makes the fixture the contract: whatever the fixture's events table
// carries, the API must emit. But the fixture is hand-written in main_test.go,
// so a column added to canonical and not to the fixture leaves both tests green
// and the API silently short a field. That is exactly how `category` shipped —
// the suite was self-consistent and wrong.
//
// Skipped unless a real artifact is named, because the suite must keep running
// offline:
//
//	BITCAL_CANONICAL_DB=~/code/dump/projects/21ideas/calendar/dbs/events_ru.db \
//	  go test -tags fts5 -run TestFixtureSchemaMatchesCanonical ./tests
//
// Wired into publish-db.sh, which already holds the validated files at release
// time — the one moment a schema change is expected to be reviewed.
func TestFixtureSchemaMatchesCanonical(t *testing.T) {
	path := os.Getenv("BITCAL_CANONICAL_DB")
	if path == "" {
		t.Skip("set BITCAL_CANONICAL_DB to a real artifact to compare schemas")
	}

	canonical := set(eventColumns(t, path))
	fixture := set(eventColumns(t, filepath.Join(artifactDir, "events_ru.db")))

	for col := range canonical {
		if !fixture[col] {
			t.Errorf("canonical has a %q column the test fixture does not model.\n"+
				"The suite cannot notice a field the API fails to emit if its own\n"+
				"fixture has no such column. Add it to seedArtifact in main_test.go,\n"+
				"then let TestEventStructCoversEveryColumn tell you what else to fix.", col)
		}
	}
	for col := range fixture {
		if !canonical[col] {
			t.Errorf("the fixture models a %q column canonical no longer has; the\n"+
				"suite is pinning a schema that does not exist, and Test A would\n"+
				"require the API to emit a column it cannot read.", col)
		}
	}
}

// eventColumns reads the column names of the events table straight out of the
// artifact. Names, not positions: the two languages genuinely order their
// columns differently — RU stores media fourth, EN eighth — so anything
// positional would be correct on one artifact and wrong on the other.
func eventColumns(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name FROM pragma_table_info('events')`)
	if err != nil {
		t.Fatalf("pragma table_info(events): %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scanning column name: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading column names: %v", err)
	}
	return names
}

func set(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
