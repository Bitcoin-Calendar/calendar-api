package tests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// halvingRow is one extra row for the search filter tests. They are seeded into
// an artifact of their own by seedHalvingRows rather than into fixtureRows, so
// the suite-wide counts other tests pin stay as they are and none of these
// requests is spent from the shared instance's rate-limit budget.
type halvingRow struct {
	id       int
	date     string
	strong   bool // carries the term often enough to outrank every other row
	category string
	landmark bool
}

// Every row matches q=halving and no base fixture row does, so the full match
// set for a search is exactly the rows listed here.
//
// The weak rows share their indexed text word for word, which gives them the
// same bm25 score: their order is decided by e.id alone, and must be id
// descending. The one strong row has the lowest id, so an id-only sort would put
// it last and only fts.rank can put it first. Together they pin both halves of
// ORDER BY fts.rank, e.id DESC.
//
// The dates span years and months on purpose. Search reads the whole corpus, and
// a filter that quietly narrowed it to one date range would lose most of these.
var halvingRowsEN = []halvingRow{
	{101, "2012-11-28", true, "holiday", true},
	{102, "2016-07-09", false, "archives", false},
	{103, "2020-05-11", false, "holiday", false},
	{104, "2024-04-20", false, "archives", true},
	{105, "2019-02-14", false, "mustread", false},
	{106, "2021-10-03", false, "archives", false},
}

// The Russian set differs in size and uses `bitcoin`, a category only the RU
// artifact carries, so an answer read from the wrong artifact or validated
// against the wrong vocabulary cannot pass.
var halvingRowsRU = []halvingRow{
	{101, "2012-11-28", true, "holiday", true},
	{102, "2016-07-09", false, "bitcoin", false},
	{103, "2020-05-11", false, "bitcoin", true},
}

// seedHalvingRows is a stageArtifact mutation. It runs against both languages,
// so it tells them apart by the row only the RU fixture carries.
func seedHalvingRows(db *sql.DB) error {
	var ru int
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE url_path = '/2020-12-08/ru-only/'`,
	).Scan(&ru); err != nil {
		return err
	}
	rows := halvingRowsEN
	if ru == 1 {
		rows = halvingRowsRU
	}

	for _, r := range rows {
		title, description := "Halving day", "The block subsidy is cut in half on this day."
		if r.strong {
			title, description = "Halving halving halving", "Halving, and halving again."
		}
		if _, err := db.Exec(
			`INSERT INTO events (id, date, title, description, tags, url_path, category, landmark)
			 VALUES (?,?,?,?,?,?,?,?)`,
			r.id, r.date, title, description, `["halving"]`,
			fmt.Sprintf("/%s/halving-%d/", r.date, r.id), r.category, r.landmark,
		); err != nil {
			return fmt.Errorf("inserting halving row %d: %w", r.id, err)
		}
	}
	return nil
}

// searchAnswer is one /api/search response, kept raw enough to tell an empty
// events array from a null one.
type searchAnswer struct {
	code      int
	list      eventList
	rawEvents string
	errMsg    string
}

func searchFrom(t *testing.T, base, query string) searchAnswer {
	t.Helper()
	var body struct {
		Events     json.RawMessage `json:"events"`
		Pagination json.RawMessage `json:"pagination"`
		Error      string          `json:"error"`
	}
	code := getFrom(t, base, "/api/search?"+query, &body)
	a := searchAnswer{code: code, rawEvents: string(body.Events), errMsg: body.Error}
	if code == http.StatusOK {
		if err := json.Unmarshal(body.Events, &a.list.Events); err != nil {
			t.Fatalf("%s: events is not an array of events: %v", query, err)
		}
		if err := json.Unmarshal(body.Pagination, &a.list.Pagination); err != nil {
			t.Fatalf("%s: decoding pagination: %v", query, err)
		}
	}
	return a
}

func eventIDs(events []event) []int {
	ids := []int{}
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	return ids
}

// expectSearch asserts a 200 carrying exactly these events, in this order, and
// this pagination. The order is part of the assertion: rank then id is the
// contract, and a filter applied after the sort — or after LIMIT — shows up as
// the wrong rows on the wrong page rather than only as a wrong total.
func expectSearch(t *testing.T, base, query string, wantIDs []int, wantTotal, wantLastPage int) searchAnswer {
	t.Helper()
	a := searchFrom(t, base, query)
	if a.code != http.StatusOK {
		t.Fatalf("%s: want 200, got %d: %s", query, a.code, a.errMsg)
	}
	if a.rawEvents == "null" {
		t.Errorf("%s: events is null; every list endpoint answers [] when nothing matches", query)
	}
	if got := eventIDs(a.list.Events); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("%s: want events %v, got %v", query, wantIDs, got)
	}
	if a.list.Pagination.Total != wantTotal {
		t.Errorf("%s: want total %d, got %d — the total must count the filtered matches, "+
			"not the unfiltered ones", query, wantTotal, a.list.Pagination.Total)
	}
	if a.list.Pagination.LastPage != wantLastPage {
		t.Errorf("%s: want last_page %d, got %d", query, wantLastPage, a.list.Pagination.LastPage)
	}
	return a
}

// TestSearchFilters pins ?category= and ?landmark= on /api/search: categories
// OR among themselves, landmark ANDs with them, both AND with the full-text
// match, and the total and the page are answered from the same filtered set.
//
// It boots its own instance on an artifact carrying the halving rows, so the
// exact results below depend on nothing any other test does.
func TestSearchFilters(t *testing.T) {
	dir := stageArtifact(t, seedHalvingRows)
	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused the halving artifact: %v\n--- log ---\n%s", startErr, serviceLog)
	}

	// The control, and the compatibility promise: with no filter, search answers
	// exactly what it always did.
	t.Run("no filters keeps the unfiltered answer", func(t *testing.T) {
		all := expectSearch(t, base, "lang=en&q=halving", []int{101, 106, 105, 104, 103, 102}, 6, 1)
		expectSearch(t, base, "lang=en&q=halving&limit=2&page=2", []int{105, 104}, 6, 3)

		// Full corpus, not a date window: the matches span six different months
		// across four years, and all of them come back.
		months := map[string]bool{}
		for _, e := range all.list.Events {
			if len(e.Date) < len("2006-01") {
				t.Fatalf("event %d carries a malformed date %q", e.ID, e.Date)
			}
			months[e.Date[:len("2006-01")]] = true
		}
		if len(months) != 6 {
			t.Errorf("q=halving: want matches from 6 distinct months, got %d (%v)", len(months), months)
		}
	})

	// Rank first, id second, with filters applied. 101 has the lowest id and
	// must still lead; the equally-ranked rows after it must run id descending.
	t.Run("rank orders before id and ties break by id descending", func(t *testing.T) {
		expectSearch(t, base, "lang=en&q=halving&category=archives&category=holiday",
			[]int{101, 106, 104, 103, 102}, 5, 1)
	})

	// The filtered page reports the page and size it was cut with, beside the
	// filtered total asserted by the table below.
	t.Run("filtered page reports its position", func(t *testing.T) {
		a := expectSearch(t, base, "lang=en&q=halving&category=archives&category=holiday&limit=2&page=2",
			[]int{104, 103}, 5, 3)
		if a.list.Pagination.CurrentPage != 2 || a.list.Pagination.PerPage != 2 {
			t.Errorf("want current_page 2 and per_page 2, got %d and %d",
				a.list.Pagination.CurrentPage, a.list.Pagination.PerPage)
		}
	})

	for _, tc := range []struct {
		name     string
		query    string
		ids      []int
		total    int
		lastPage int
	}{
		{"one category", "lang=en&q=halving&category=archives", []int{106, 104, 102}, 3, 1},
		// Normalised as /api/events normalises: trimmed and case-insensitive. A
		// value repeated, in any spelling, is still one category.
		{"category case-insensitive", "lang=en&q=halving&category=ARCHIVES", []int{106, 104, 102}, 3, 1},
		{"category padded", "lang=en&q=halving&category=%20Archives%20", []int{106, 104, 102}, 3, 1},
		{"category repeated", "lang=en&q=halving&category=archives&category=Archives&category=archives",
			[]int{106, 104, 102}, 3, 1},
		// An empty value is no filter, as `?category=` and `?landmark=` have always
		// been on /api/events; alongside a real value it is simply skipped.
		{"empty category", "lang=en&q=halving&category=", []int{101, 106, 105, 104, 103, 102}, 6, 1},
		{"empty category beside a real one", "lang=en&q=halving&category=&category=archives",
			[]int{106, 104, 102}, 3, 1},
		{"empty landmark", "lang=en&q=halving&landmark=", []int{101, 106, 105, 104, 103, 102}, 6, 1},
		{"repeated categories OR", "lang=en&q=halving&category=archives&category=holiday",
			[]int{101, 106, 104, 103, 102}, 5, 1},
		{"category AND landmark=true", "lang=en&q=halving&category=archives&landmark=true",
			[]int{104}, 1, 1},
		{"categories AND landmark=true", "lang=en&q=halving&category=archives&category=holiday&landmark=true",
			[]int{101, 104}, 2, 1},
		{"categories AND landmark=false", "lang=en&q=halving&category=archives&category=holiday&landmark=false",
			[]int{106, 103, 102}, 3, 1},
		// Base fixture events 1 and 2 are landmarks too; they must not appear,
		// because the flag narrows the full-text match rather than joining it.
		{"landmark=true alone", "lang=en&q=halving&landmark=true", []int{101, 104}, 2, 1},
		{"landmark=false alone", "lang=en&q=halving&landmark=false", []int{106, 105, 103, 102}, 4, 1},
		// The same spellings /api/events accepts: strconv.ParseBool, after trimming.
		{"landmark=1", "lang=en&q=halving&landmark=1", []int{101, 104}, 2, 1},
		{"landmark padded", "lang=en&q=halving&landmark=%20true%20", []int{101, 104}, 2, 1},

		// Filtered pagination: the page is cut from the filtered set. Filtering
		// after LIMIT would hand back fewer rows than asked for, and different ones.
		{"filtered second page", "lang=en&q=halving&category=archives&category=holiday&limit=2&page=2",
			[]int{104, 103}, 5, 3},
		{"filtered page past the end", "lang=en&q=halving&category=archives&category=holiday&limit=2&page=4",
			[]int{}, 5, 3},

		// Valid filters with no match are a 200 with an empty array, not a 400:
		// mustread is in the vocabulary, it just has no landmark halving.
		{"filters that match nothing", "lang=en&q=halving&category=mustread&landmark=true", []int{}, 0, 0},
		{"filters over a query that matches nothing", "lang=en&q=zzzznotathing&category=archives",
			[]int{}, 0, 0},

		// Locale: each language filters its own artifact against its own
		// vocabulary, and an unknown lang is English.
		{"ru category", "lang=ru&q=halving&category=bitcoin", []int{103, 102}, 2, 1},
		{"ru landmark", "lang=ru&q=halving&landmark=true", []int{101, 103}, 2, 1},
		// mustread is in the RU vocabulary, and on no RU halving row. The same
		// request in English answers [105], so a 200 with that row is the wrong
		// artifact.
		{"ru category without ru matches", "lang=ru&q=halving&category=mustread", []int{}, 0, 0},
		{"unknown lang falls back to en", "lang=xx&q=halving&category=archives", []int{106, 104, 102}, 3, 1},
		{"absent lang is en", "q=halving&category=archives", []int{106, 104, 102}, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectSearch(t, base, tc.query, tc.ids, tc.total, tc.lastPage)
		})
	}

	// Rejections. Each must be a 400 that explains the value, not the refusal the
	// endpoint used to give every filter — that message would be a lie once the
	// filters exist.
	for _, tc := range []struct {
		name    string
		query   string
		mention []string
	}{
		// The vocabulary is named so the caller can fix the request.
		{"unknown category", "lang=en&q=halving&category=nonesuch", []string{"nonesuch", "archives", "holiday"}},
		// Every repeated value is validated, not only the first.
		{"unknown category after a valid one", "lang=en&q=halving&category=archives&category=nonesuch",
			[]string{"nonesuch"}},
		{"unknown category before a valid one", "lang=en&q=halving&category=nonesuch&category=archives",
			[]string{"nonesuch"}},
		// Only spaces is not empty: trimmed, it names no category, as on /api/events.
		{"blank category", "lang=en&q=halving&category=%20", []string{"category", "archives"}},
		// bitcoin is a real category, in the other artifact.
		{"ru-only category in en", "lang=en&q=halving&category=bitcoin", []string{"bitcoin", "archives"}},
		{"ru-only category under the en fallback", "lang=xx&q=halving&category=bitcoin", []string{"bitcoin"}},

		{"landmark=yes", "lang=en&q=halving&landmark=yes", []string{"landmark", "true or false"}},
		{"landmark=2", "lang=en&q=halving&landmark=2", []string{"landmark", "true or false"}},
		{"landmark blank", "lang=en&q=halving&landmark=%20", []string{"landmark", "true or false"}},
		{"invalid landmark with a valid category", "lang=en&q=halving&category=archives&landmark=yes",
			[]string{"landmark", "true or false"}},

		// A malformed expression is still the caller's error with filters present.
		{"malformed query with filters", "lang=en&q=AND&category=archives&landmark=true",
			[]string{"full-text search expression"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := searchFrom(t, base, tc.query)
			if a.code != http.StatusBadRequest {
				t.Fatalf("%s: want 400, got %d", tc.query, a.code)
			}
			for _, m := range tc.mention {
				if !strings.Contains(a.errMsg, m) {
					t.Errorf("%s: the rejection %q does not mention %q", tc.query, a.errMsg, m)
				}
			}
			if strings.Contains(a.errMsg, "is not a filter") {
				t.Errorf("%s: rejected as an unsupported parameter (%q); search supports it now",
					tc.query, a.errMsg)
			}
		})
	}
}

// TestSearchFiltersOnArtifactsWithoutOptionalColumns is the rollback case. An
// artifact predating `category` and `landmark` must still answer an unfiltered
// search, and must refuse each filter by saying the artifact predates the column
// rather than failing with `no such column`.
func TestSearchFiltersOnArtifactsWithoutOptionalColumns(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		if err := seedHalvingRows(db); err != nil {
			return err
		}
		for _, s := range []string{
			`ALTER TABLE events DROP COLUMN category`,
			`ALTER TABLE events DROP COLUMN landmark`,
		} {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})
	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused a rollback artifact: %v\n--- log ---\n%s", startErr, serviceLog)
	}

	expectSearch(t, base, "lang=en&q=halving", []int{101, 106, 105, 104, 103, 102}, 6, 1)

	for _, query := range []string{
		"lang=en&q=halving&category=archives",
		"lang=en&q=halving&landmark=true",
	} {
		a := searchFrom(t, base, query)
		if a.code != http.StatusBadRequest {
			t.Errorf("%s on an artifact without the column: want 400, got %d", query, a.code)
			continue
		}
		if !strings.Contains(a.errMsg, "predates") {
			t.Errorf("%s: the rejection %q does not say the artifact predates the column", query, a.errMsg)
		}
	}
}
