package tests

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The fixture's landmark split, named once. Events 1 and 2 carry the flag in
// both languages; RU has one extra row that does not, so the false counts
// differ. See fixtureRows.
const (
	landmarksRU    = 2
	notLandmarksRU = 3
	landmarksEN    = 2
	notLandmarksEN = 2
)

// TestLandmarkFilter is the happy path: the parameter narrows the list, both
// polarities work, and every row that comes back really carries the value asked
// for.
//
// The last of those is what stops this passing against a handler that ignores
// the parameter. A count alone would be satisfied by any filter that happened to
// return the right number of rows; checking the flag on each returned event ties
// the answer to the column.
func TestLandmarkFilter(t *testing.T) {
	for _, tc := range []struct {
		lang  string
		want  bool
		total int
	}{
		{"ru", true, landmarksRU},
		{"ru", false, notLandmarksRU},
		{"en", true, landmarksEN},
		{"en", false, notLandmarksEN},
	} {
		name := tc.lang + "/"
		if tc.want {
			name += "true"
		} else {
			name += "false"
		}
		t.Run(name, func(t *testing.T) {
			var list eventList
			path := "/api/events?lang=" + tc.lang + "&limit=100&landmark="
			if tc.want {
				path += "true"
			} else {
				path += "false"
			}
			if code := getAs(t, apiKey2, path, &list); code != http.StatusOK {
				t.Fatalf("%s: want 200, got %d", path, code)
			}
			if list.Pagination.Total != tc.total {
				t.Errorf("%s: want %d events, got %d", path, tc.total, list.Pagination.Total)
			}
			if len(list.Events) == 0 {
				t.Fatalf("%s returned no events; the assertion below would pass vacuously", path)
			}
			for _, e := range list.Events {
				if e.Landmark != tc.want {
					t.Errorf("event %d came back under landmark=%v but carries %v — the filter is "+
						"not reading the column it claims to", e.ID, tc.want, e.Landmark)
				}
			}
		})
	}
}

// TestLandmarkFilterIsNotACategoryFilter guards the pairing that a handler
// treating the two fields as interchangeable would get wrong.
//
// The fixture is built so the two cannot be confused: events 1 and 2 are the
// landmarks and they carry *different* categories (holiday and mustread), while
// event 3 shares neither. So no single category selects the landmark set and no
// landmark value selects a category, and a filter wired to the wrong column
// cannot accidentally answer correctly.
func TestLandmarkFilterIsNotACategoryFilter(t *testing.T) {
	var landmarks eventList
	if code := getAs(t, apiKey2, "/api/events?lang=ru&limit=100&landmark=true", &landmarks); code != http.StatusOK {
		t.Fatalf("landmark=true: want 200, got %d", code)
	}
	seen := map[string]bool{}
	for _, e := range landmarks.Events {
		seen[e.Category] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every landmark in the fixture shares a category (%v); the fixture no longer "+
			"models the case this guards", seen)
	}

	// And the converse: a category that contains one landmark and one row that
	// is not, so that ?category= alone cannot stand in for ?landmark=.
	var holiday eventList
	if code := getAs(t, apiKey2, "/api/events?lang=ru&limit=100&category=holiday", &holiday); code != http.StatusOK {
		t.Fatalf("category=holiday: want 200, got %d", code)
	}
	if holiday.Pagination.Total == 0 {
		t.Fatal("category=holiday matched nothing")
	}
}

// TestLandmarkComposesWithOtherFilters pins that the filters AND together. The
// website's switch is meant to narrow whatever the calendar is already showing,
// so a landmark filter that silently replaced the category or date filter would
// be worse than none.
func TestLandmarkComposesWithOtherFilters(t *testing.T) {
	// Event 1 is 1881-09-29, category holiday, landmark true.
	var hit eventList
	if code := getAs(t, apiKey2,
		"/api/events?lang=ru&category=holiday&landmark=true&month=9&day=29", &hit); code != http.StatusOK {
		t.Fatalf("category+landmark+date: want 200, got %d", code)
	}
	if hit.Pagination.Total != 1 {
		t.Errorf("category=holiday&landmark=true&month=9&day=29: want 1, got %d", hit.Pagination.Total)
	}

	// The same row, asked for with the flag it does not carry. If the filters
	// ORed, or if landmark were ignored, this would still be 1.
	var miss eventList
	if code := getAs(t, apiKey2,
		"/api/events?lang=ru&category=holiday&landmark=false&month=9&day=29", &miss); code != http.StatusOK {
		t.Fatalf("category+landmark+date: want 200, got %d", code)
	}
	if miss.Pagination.Total != 0 {
		t.Errorf("category=holiday&landmark=false&month=9&day=29: want 0, got %d — the filters "+
			"are not ANDing", miss.Pagination.Total)
	}
}

// TestLandmarkRejectsUnparseableValues covers the reason this parameter
// validates at all.
//
// A ?landmark=yes quietly read as false is the failure the date filters and
// ?category= were both hardened against: it answers 200 with a plausible body —
// here, precisely the rows the caller least wanted — and nothing in the response
// says the filter was not the one asked for.
func TestLandmarkRejectsUnparseableValues(t *testing.T) {
	for _, bad := range []string{"yes", "no", "on", "2", "-1", "true false", "тру", " "} {
		t.Run(url.QueryEscape(bad), func(t *testing.T) {
			var body struct {
				Error string `json:"error"`
			}
			code := getAs(t, apiKey2, "/api/events?lang=ru&landmark="+url.QueryEscape(bad), &body)
			if code != http.StatusBadRequest {
				t.Fatalf("landmark=%q: want 400, got %d", bad, code)
			}
			if !strings.Contains(body.Error, "true or false") {
				t.Errorf("landmark=%q was rejected with %q, which does not say what would be "+
					"accepted; a caller cannot fix the request from that", bad, body.Error)
			}
		})
	}
}

// TestLandmarkAcceptsTheFormsParseBoolDoes pins the accepted spellings, because
// the rejection message names only `true or false` and a reader could reasonably
// conclude the rest are refused. They are not, and a client sending 1 or 0 —
// which is what the column literally holds — must not get a 400.
func TestLandmarkAcceptsTheFormsParseBoolDoes(t *testing.T) {
	for _, form := range []struct {
		sent  string
		total int
	}{
		{"true", landmarksRU}, {"True", landmarksRU}, {"TRUE", landmarksRU},
		{"1", landmarksRU}, {"t", landmarksRU},
		{"false", notLandmarksRU}, {"False", notLandmarksRU},
		{"0", notLandmarksRU}, {"f", notLandmarksRU},
	} {
		t.Run(form.sent, func(t *testing.T) {
			var list eventList
			code := getAs(t, apiKey2, "/api/events?lang=ru&limit=100&landmark="+form.sent, &list)
			if code != http.StatusOK {
				t.Fatalf("landmark=%s: want 200, got %d", form.sent, code)
			}
			if list.Pagination.Total != form.total {
				t.Errorf("landmark=%s: want %d, got %d", form.sent, form.total, list.Pagination.Total)
			}
		})
	}
}

// TestLandmarkParamRefusedWhereItDoesNotFilter is the ?category= guard's
// argument applied to the new parameter, and it is here from the start rather
// than after the fact: /api/search and /api/events/tags/:tag do not narrow by
// landmark, and accepting the parameter while ignoring it would answer 200 with
// every match and nothing to say the filter had not been applied.
func TestLandmarkParamRefusedWhereItDoesNotFilter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted string
		rejected string
	}{
		{
			name:     "search",
			accepted: "/api/search?lang=ru&q=satoshi",
			rejected: "/api/search?lang=ru&q=satoshi&landmark=true",
		},
		{
			name:     "by-tag",
			accepted: "/api/events/tags/satoshi?lang=ru",
			rejected: "/api/events/tags/satoshi?lang=ru&landmark=true",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control: without the parameter the endpoint answers, so the 400
			// below is about the parameter and not about the request.
			var list eventList
			if code := getAs(t, apiKey2, tc.accepted, &list); code != http.StatusOK {
				t.Fatalf("%s: want 200, got %d", tc.accepted, code)
			}
			if len(list.Events) == 0 {
				t.Fatalf("%s matched nothing; this test would prove nothing", tc.accepted)
			}

			var body struct {
				Error string `json:"error"`
			}
			if code := getAs(t, apiKey2, tc.rejected, &body); code != http.StatusBadRequest {
				t.Fatalf("%s: want 400, got %d — the parameter was accepted and ignored, which "+
					"is a filtered-looking response that was never filtered", tc.rejected, code)
			}
			if !strings.Contains(body.Error, "landmark") {
				t.Errorf("%s was rejected with %q, which does not name the parameter at fault",
					tc.rejected, body.Error)
			}
		})
	}
}

// TestHealthReportsTheLandmarksItServes is publish-db.sh's assertion, the way
// TestHealthReportsTheVocabularyItServes is.
//
// The count is checked against what the filter actually returns rather than
// against a number written here, for the same reason: what has to be true is
// that /health describes the artifact the service is really filtering on, and
// the filter is where that is observable. A literal would pass just as well
// against a field populated from a second query.
func TestHealthReportsTheLandmarksItServes(t *testing.T) {
	var health healthDoc
	fetchJSON(t, baseURL+"/health", &health)

	for _, lang := range []string{"en", "ru"} {
		t.Run(lang, func(t *testing.T) {
			db, found := health.Databases[lang]
			if !found {
				t.Fatalf("%s: absent from /health", lang)
			}
			if !db.Landmark.Present {
				t.Fatalf("%s: /health reports no landmark column, but the fixture has one", lang)
			}

			var list eventList
			if code := getAs(t, apiKey3, "/api/events?lang="+lang+"&limit=100&landmark=true", &list); code != http.StatusOK {
				t.Fatalf("landmark=true: want 200, got %d", code)
			}
			if list.Pagination.Total == 0 {
				t.Fatal("no landmarks came back, so the count below proves nothing")
			}
			if db.Landmark.Count != int64(list.Pagination.Total) {
				t.Errorf("%s: /health says %d landmarks, ?landmark=true returns %d — the release "+
					"check would be asserting against a number the service does not filter by",
					lang, db.Landmark.Count, list.Pagination.Total)
			}
		})
	}
}

// TestServiceBootsWithoutALandmarkColumn is the rollback test, and it is the
// whole reason the presence probe exists.
//
// `landmark` arrived on 2026-08-12. Every release before that has no such
// column — which, at the time this was written, was every artifact on the box —
// and those files are rollback targets: publish-db.sh's rollback() re-points
// `current` at the previous release and restarts. The lesson was already paid
// for once by `category`, whose absence made this binary refuse to start and
// turned a rollback into an outage.
//
// Measured before this test was written, against a copy of the real RU artifact
// with the column dropped: GORM's own statements survive untouched — they SELECT
// * and leave the struct field at its zero value — while `WHERE landmark = ?`
// fails with `no such column: landmark`. So the parts that need guarding are
// exactly the two that name the column by hand, and this asserts both.
func TestServiceBootsWithoutALandmarkColumn(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`ALTER TABLE events DROP COLUMN landmark`)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact predating the landmark "+
			"column. That artifact is a rollback target, and everything except ?landmark= "+
			"works perfectly well on it: %v\n--- log ---\n%s", startErr, serviceLog)
	}
	if !strings.Contains(serviceLog, "no landmark column") {
		t.Errorf("the service degraded silently; nothing in its log says the artifact has no "+
			"landmark column:\n%s", serviceLog)
	}

	// Everything not keyed on the column still works.
	var list eventList
	if code := getFrom(t, base, "/api/events?lang=ru&limit=100", &list); code != http.StatusOK {
		t.Fatalf("/api/events: want 200, got %d", code)
	}
	if list.Pagination.Total != 5 {
		t.Errorf("/api/events total: want 5, got %d", list.Pagination.Total)
	}
	// The field still renders, as false, on rows that carried it before the
	// rollback. That is the documented contract on such an artifact and the
	// reason the field is a bool rather than a pointer — so pin it, because the
	// alternative shapes (a missing key, or null) are what a *bool would give.
	for _, e := range list.Events {
		if e.Landmark {
			t.Errorf("event %d reports landmark true on an artifact with no landmark column",
				e.ID)
		}
	}

	// Search especially. It is the one handler that enumerates its columns by
	// hand instead of letting GORM derive them, so it is the one that would name
	// e.landmark into a table that has none — and `category` proved that failure
	// answers 500 to every query while the rest of the service is fine. Booting
	// and then failing every search is not a rollback that worked.
	var found eventList
	if code := getFrom(t, base, "/api/search?lang=ru&q=satoshi&limit=100", &found); code != http.StatusOK {
		t.Errorf("/api/search: want 200, got %d — the hand-written SELECT in "+
			"ftsSearchHandler still names a column this artifact does not have", code)
	}
	if len(found.Events) == 0 {
		t.Error("/api/search returned nothing for a term the fixture carries; a 200 with an " +
			"empty list is the failure this would otherwise hide")
	}

	// The filter cannot be answered, so it must be refused. 200 would be an empty
	// list indistinguishable from an artifact with no landmarks, and 500 would be
	// a server error for a question the server can answer — which is exactly what
	// an unguarded `WHERE landmark = ?` produces here.
	for _, sent := range []string{"true", "false"} {
		var errBody struct {
			Error string `json:"error"`
		}
		code := getFrom(t, base, "/api/events?lang=ru&landmark="+sent, &errBody)
		if code != http.StatusBadRequest {
			t.Errorf("?landmark=%s against an artifact with no landmark column: want 400, got %d",
				sent, code)
		}
		if !strings.Contains(errBody.Error, "predates") {
			t.Errorf("?landmark=%s: the rejection does not explain that the artifact predates "+
				"the column: %q", sent, errBody.Error)
		}
	}

	// /health must say the column is absent. Serving this artifact is a correct
	// outcome — it is what a rollback looks like — and the release check treats
	// absent and empty differently.
	var health healthDoc
	fetchJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("status: want ok, got %q — a rollback target is not a degraded service",
			health.Status)
	}
	for lang, db := range health.Databases {
		if db.Landmark.Present {
			t.Errorf("%s: /health claims a landmark column on an artifact that has none", lang)
		}
		if db.Landmark.Count != 0 {
			t.Errorf("%s: /health reports %d landmarks with no column to hold them",
				lang, db.Landmark.Count)
		}
	}
}

// TestNoLandmarksIsReportedNotFatal covers the artifact that has the column and
// carries the flag on no row.
//
// Unlike an empty category vocabulary, this breaks no upstream invariant:
// validate.py invariant 14 pins every value to 0 or 1 and deliberately sets no
// target fraction, because the flag is an editorial judgement. So it is legal
// data — and it would still empty the one UI control the column exists for, with
// nothing upstream to notice. The service must serve it, say so, and report it
// where a release can see it.
//
// Note what is *not* asserted: ?landmark=true is a 200 with an empty list here,
// not a 400. The vocabulary case rejects because no value could ever match; this
// one has a perfectly answerable question with a genuinely empty answer, and
// refusing it would be the API disagreeing with its own data.
func TestNoLandmarksIsReportedNotFatal(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE events SET landmark = 0`)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact carrying no landmarks, "+
			"which is legal data: %v\n--- log ---\n%s", startErr, serviceLog)
	}
	// Distinguishes this from the missing-column path, which also answers no
	// landmarks — without it this test would pass for the wrong reason.
	if !strings.Contains(serviceLog, "no landmarks") {
		t.Errorf("the service degraded silently; nothing in its log says the column carries "+
			"the flag on no row:\n%s", serviceLog)
	}

	var list eventList
	if code := getFrom(t, base, "/api/events?lang=ru&limit=100&landmark=true", &list); code != http.StatusOK {
		t.Fatalf("?landmark=true: want 200, got %d — the column is present and the question is "+
			"answerable, so an empty answer is the honest one", code)
	}
	if list.Pagination.Total != 0 {
		t.Errorf("?landmark=true: want 0, got %d", list.Pagination.Total)
	}

	var health healthDoc
	fetchJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("status: want ok — no landmarks is not an index problem, got %q", health.Status)
	}
	for lang, db := range health.Databases {
		if !db.Landmark.Present {
			t.Errorf("%s: /health says the column is absent; it is present and unset, which is "+
				"a different state with a different fix", lang)
		}
		if db.Landmark.Count != 0 {
			t.Errorf("%s: /health reports %d landmarks on an artifact where every row is 0",
				lang, db.Landmark.Count)
		}
	}
}
