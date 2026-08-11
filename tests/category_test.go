package tests

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type categoryInfo struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// getFrom is getAs against a service other than the suite's own, for the tests
// that boot an instance on a deliberately odd artifact.
func getFrom(t *testing.T, base, path string, into interface{}) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-API-KEY", apiKey)

	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	if into != nil && strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(res.Body).Decode(into)
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode
}

// TestCategoryFilter covers the filter's happy path against the fixture, whose
// categories are deliberately not equal to any row's first tag — a filter that
// accidentally matched on tags would pass a fixture where the two agreed.
func TestCategoryFilter(t *testing.T) {
	// Fixture categories, RU: holiday(1), mustread(1), archives(1), bitcoin(1),
	// and syntheticCategory(1). `bitcoin` is the important one: it is a category
	// carried by a row whose tags do not include it, which is the exact shape
	// canonical has after the tag was retired.
	for _, tc := range []struct {
		category string
		want     int
	}{
		{"holiday", 1},
		{"archives", 1},
		{"bitcoin", 1},
		{syntheticCategory, 1},
	} {
		t.Run(tc.category, func(t *testing.T) {
			var list eventList
			if code := getAs(t, apiKey2, "/api/events?lang=ru&category="+tc.category, &list); code != http.StatusOK {
				t.Fatalf("want 200, got %d", code)
			}
			if list.Pagination.Total != tc.want {
				t.Errorf("category=%s: want %d events, got %d", tc.category, tc.want, list.Pagination.Total)
			}
			for _, e := range list.Events {
				if e.Category != tc.category {
					t.Errorf("event %d came back under category=%s but carries %q", e.ID, tc.category, e.Category)
				}
			}
		})
	}
}

// TestCategoryFilterIsNotTagFilter is the assertion that the two are wired to
// different columns. Fixture event 4 (RU) is category `bitcoin` and tagged
// `satoshi`; no row is tagged `bitcoin` at all, exactly as in canonical.
func TestCategoryFilterIsNotTagFilter(t *testing.T) {
	var byCategory, byTag eventList
	if code := getAs(t, apiKey2, "/api/events?lang=ru&category=bitcoin", &byCategory); code != http.StatusOK {
		t.Fatalf("category=bitcoin: want 200, got %d", code)
	}
	if code := getAs(t, apiKey2, "/api/events/tags/bitcoin?lang=ru", &byTag); code != http.StatusOK {
		t.Fatalf("tags/bitcoin: want 200, got %d", code)
	}
	if byCategory.Pagination.Total == 0 {
		t.Fatal("category=bitcoin matched nothing; the fixture no longer models the case this guards")
	}
	if byTag.Pagination.Total != 0 {
		t.Errorf("tags/bitcoin matched %d events: `bitcoin` is a category, not a tag",
			byTag.Pagination.Total)
	}
}

// TestUnknownCategoryIsRejected is the same argument as the date filters: an
// unknown value must not answer 200 with an empty list, because that is
// indistinguishable from a category that genuinely has no events, and a client
// cannot tell its own typo from a quiet corner of the corpus.
func TestUnknownCategoryIsRejected(t *testing.T) {
	// What the artifact carries, asked of the service rather than listed here,
	// so this cannot drift from the fixture. Every one of these must appear in
	// the rejection message.
	var vocab struct {
		Data []categoryInfo `json:"data"`
	}
	if code := getAs(t, apiKey2, "/api/categories?lang=ru", &vocab); code != http.StatusOK {
		t.Fatalf("/api/categories: want 200, got %d", code)
	}
	if len(vocab.Data) == 0 {
		t.Fatal("the fixture reports no categories, so the assertions below would pass vacuously")
	}

	for _, bad := range []string{"nonesuch", "bitcion", "Bitcoin Core", "'; DROP TABLE events;--", "%"} {
		t.Run(bad, func(t *testing.T) {
			var body struct {
				Error string `json:"error"`
			}
			code := getAs(t, apiKey2, "/api/events?lang=ru&category="+url.QueryEscape(bad), &body)
			if code != http.StatusBadRequest {
				t.Fatalf("category=%q: want 400, got %d", bad, code)
			}
			// The message must name what would have been accepted. A 400 that
			// only says "invalid" leaves the caller guessing at a vocabulary
			// they cannot see — and asserting merely that the message is
			// non-empty would accept exactly that, which is why this checks the
			// vocabulary is in it rather than checking that something was said.
			for _, ci := range vocab.Data {
				if !strings.Contains(body.Error, ci.Category) {
					t.Errorf("category=%q was rejected with %q, which does not name %q. A caller "+
						"who cannot see the vocabulary has to guess at it from this message.",
						bad, body.Error, ci.Category)
				}
			}
			// And it must echo what was rejected, or a caller batching requests
			// cannot tell which one the message is about.
			if !strings.Contains(body.Error, bad) {
				t.Errorf("category=%q was rejected with %q, which does not repeat the value sent",
					bad, body.Error)
			}
		})
	}
}

// TestCategoryIsRejectedWhereItIsNotAFilter covers the two endpoints that return
// events and do not filter by category.
//
// Both accepted the parameter and ignored it: &category=bitcoin on a search
// answered 200 with every match, and nothing in that response distinguishes it
// from a filter that ran. That is the silent empty result's mirror image — a
// silent *unfiltered* result — and the same argument applies, so the parameter
// is refused where it does nothing.
//
// The controls matter here: without them a handler that rejected every request
// would pass.
func TestCategoryIsRejectedWhereItIsNotAFilter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ok       string // must answer 200 and return events
		rejected string // the same request with a category appended
	}{
		{
			name:     "search",
			ok:       "/api/search?lang=ru&q=satoshi",
			rejected: "/api/search?lang=ru&q=satoshi&category=bitcoin",
		},
		{
			name:     "events by tag",
			ok:       "/api/events/tags/satoshi?lang=ru",
			rejected: "/api/events/tags/satoshi?lang=ru&category=bitcoin",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var list eventList
			if code := getAs(t, apiKey3, tc.ok, &list); code != http.StatusOK {
				t.Fatalf("%s: want 200, got %d", tc.ok, code)
			}
			if len(list.Events) == 0 {
				t.Fatalf("%s returned no events, so the rejection below proves nothing", tc.ok)
			}

			var body struct {
				Error string `json:"error"`
			}
			code := getAs(t, apiKey3, tc.rejected, &body)
			if code != http.StatusBadRequest {
				t.Fatalf("%s: want 400, got %d — the parameter is not honoured here, and "+
					"answering 200 leaves the caller unable to tell that their filter was "+
					"dropped", tc.rejected, code)
			}
			// `bitcoin` is a real category in the ru artifact, so this is not a
			// vocabulary rejection wearing the wrong coat: the endpoint has to
			// refuse the parameter itself.
			if !strings.Contains(body.Error, "category") {
				t.Errorf("the rejection does not name the parameter: %q", body.Error)
			}
			if !strings.Contains(body.Error, "/api/events") {
				t.Errorf("the rejection does not say where the filter does work: %q", body.Error)
			}
		})
	}
}

// TestUnknownLangStillValidatesCategory is a regression test for a bug that
// survived the first round of these tests because every one of them named a
// real language.
//
// An unrecognised lang silently serves English — documented, and deliberate.
// But the filter validated against categoriesByLang[<raw lang>], so `lang=xx`
// found no vocabulary, treated "no vocabulary" as "nothing to check", and
// answered 200 with an empty list for a category that does not exist. One
// spelling of one parameter reopened the silent-empty-result hole the 400 was
// added to close, which is a good argument for testing the fallback path of
// anything keyed by a caller-supplied value.
func TestUnknownLangStillValidatesCategory(t *testing.T) {
	for _, lang := range []string{"xx", "EN", "de", ""} {
		t.Run("lang="+lang, func(t *testing.T) {
			var body struct {
				Error string `json:"error"`
			}
			code := getAs(t, apiKey2, "/api/events?lang="+lang+"&category=nonesuch", &body)
			if code != http.StatusBadRequest {
				t.Errorf("lang=%q category=nonesuch: want 400, got %d — an unknown category "+
					"must be rejected whatever language the caller named", lang, code)
			}
		})
	}

	// The other half: the fallback must still accept what English carries.
	var list eventList
	if code := getAs(t, apiKey2, "/api/events?lang=xx&category=mustread", &list); code != http.StatusOK {
		t.Errorf("lang=xx category=mustread: want 200, got %d", code)
	}
}

// TestCategoryFilterIsCaseInsensitive matches the documented behaviour of the
// tag filter, so the two do not disagree about how a caller may spell things.
func TestCategoryFilterIsCaseInsensitive(t *testing.T) {
	var lower, upper eventList
	getAs(t, apiKey2, "/api/events?lang=ru&category=bitcoin", &lower)
	getAs(t, apiKey2, "/api/events?lang=ru&category=BITCOIN", &upper)

	if lower.Pagination.Total == 0 {
		t.Fatal("category=bitcoin matched nothing")
	}
	if lower.Pagination.Total != upper.Pagination.Total {
		t.Errorf("case sensitivity: bitcoin=%d BITCOIN=%d",
			lower.Pagination.Total, upper.Pagination.Total)
	}
}

// TestCategoryFilterComposesWithDateFilters proves the filters AND together
// rather than one silently replacing the other.
func TestCategoryFilterComposesWithDateFilters(t *testing.T) {
	// Fixture RU event 1 is 1881-09-29, category holiday.
	var hit, miss eventList
	if code := getAs(t, apiKey2, "/api/events?lang=ru&category=holiday&month=9&day=29", &hit); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if hit.Pagination.Total != 1 {
		t.Errorf("category+date: want 1, got %d", hit.Pagination.Total)
	}
	// Same category, a date it is not on.
	if code := getAs(t, apiKey2, "/api/events?lang=ru&category=holiday&month=1&day=1", &miss); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if miss.Pagination.Total != 0 {
		t.Errorf("category+wrong date: want 0, got %d", miss.Pagination.Total)
	}
}

// TestCategoriesEndpoint pins /api/categories against the fixture, and against
// the filter: the two must not be able to disagree.
func TestCategoriesEndpoint(t *testing.T) {
	var body struct {
		Data []categoryInfo `json:"data"`
	}
	if code := getAs(t, apiKey2, "/api/categories?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(body.Data) == 0 {
		t.Fatal("/api/categories returned nothing")
	}

	// Alphabetical, like /api/tags, so a client can render it without sorting.
	for i := 1; i < len(body.Data); i++ {
		if body.Data[i-1].Category > body.Data[i].Category {
			t.Errorf("not sorted: %q before %q", body.Data[i-1].Category, body.Data[i].Category)
		}
	}

	// Every count must equal what the filter returns for that category. This
	// is the /api/tags-versus-/api/events/tags disagreement, pre-empted: one
	// counts rows, the other counts events, and for category they are the same
	// thing only because there is exactly one value per row.
	for _, ci := range body.Data {
		var list eventList
		if code := getAs(t, apiKey2, "/api/events?lang=ru&category="+ci.Category, &list); code != http.StatusOK {
			t.Fatalf("category %q: want 200, got %d", ci.Category, code)
		}
		if list.Pagination.Total != ci.Count {
			t.Errorf("category %q: /api/categories says %d, /api/events?category= says %d",
				ci.Category, ci.Count, list.Pagination.Total)
		}
	}
}

// TestVocabularyComesFromTheArtifactNotTheBinary is the one assertion that can
// distinguish this service's design — read the vocabulary out of the artifact at
// boot — from the compiled-in list it was written to avoid.
//
// Nothing else here can. Every other fixture category is a real canonical value,
// so a hardcoded list contains them all and every other test in this file passes
// against it; that was measured, not assumed. `syntheticCategory` is a value no
// such list could hold, so the filter accepts it only if loadCategories really
// did read the artifact.
//
// This is the 2026-08-10 scenario in miniature: canonical added `security` a day
// after the column shipped, and a binary carrying its own list would have
// answered 400 to a category the data already contained until someone built and
// deployed a new one.
func TestVocabularyComesFromTheArtifactNotTheBinary(t *testing.T) {
	var list eventList
	code := getAs(t, apiKey2, "/api/events?lang=ru&category="+url.QueryEscape(syntheticCategory), &list)
	if code != http.StatusOK {
		t.Fatalf("category=%q answered %d. The fixture carries this value, so the only way "+
			"to reject it is to validate against a vocabulary that did not come from the "+
			"artifact — which is the design this test exists to pin.", syntheticCategory, code)
	}
	// Without this the test would pass against a filter that accepts everything
	// and matches nothing, which is the failure mode ?category= exists to avoid.
	if list.Pagination.Total != 1 {
		t.Errorf("category=%q: want 1 event, got %d — the fixture row carrying it is gone, "+
			"and with it the only proof that the vocabulary is read from the data",
			syntheticCategory, list.Pagination.Total)
	}

	// The endpoint must report it too, or a client could never discover it.
	var body struct {
		Data []categoryInfo `json:"data"`
	}
	if code := getAs(t, apiKey2, "/api/categories?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("/api/categories: want 200, got %d", code)
	}
	for _, ci := range body.Data {
		if ci.Category == syntheticCategory {
			return
		}
	}
	t.Errorf("/api/categories does not report %q, though the artifact carries it; the "+
		"endpoint is not reading the same data the filter validates against", syntheticCategory)
}

// TestCategoryVocabularyIsPerArtifact pins that each language validates against
// its own file rather than a set shared between them.
//
// `bitcoin` is carried by RU fixture event 4 and by no EN row, so it must be a
// valid filter in one language and a 400 in the other. In production the two
// artifacts happen to carry identical category *names*, which is exactly why
// this cannot be left to canonical to demonstrate — the day they diverge is the
// day a shared vocabulary starts answering 200 with an empty list.
//
// It is also the assertion that fails if the two languages' vocabularies are
// ever loaded into the wrong slots, which the RU-superset fixture would
// otherwise hide in one direction.
func TestCategoryVocabularyIsPerArtifact(t *testing.T) {
	var ru eventList
	if code := getAs(t, apiKey2, "/api/events?lang=ru&category=bitcoin", &ru); code != http.StatusOK {
		t.Fatalf("lang=ru category=bitcoin: want 200, got %d", code)
	}
	if ru.Pagination.Total == 0 {
		t.Fatal("no ru event carries the bitcoin category; the fixture no longer models the " +
			"asymmetry this test needs")
	}

	// The same value against the artifact that does not carry it.
	for _, lang := range []string{"en", "xx"} {
		t.Run("lang="+lang, func(t *testing.T) {
			var body struct {
				Error string `json:"error"`
			}
			code := getAs(t, apiKey2, "/api/events?lang="+lang+"&category=bitcoin", &body)
			if code != http.StatusBadRequest {
				t.Errorf("lang=%s category=bitcoin: want 400, got %d — no english row carries "+
					"that category, so a 200 here means the filter validated against the "+
					"wrong artifact's vocabulary (xx falls back to english)", lang, code)
			}
		})
	}
}

// TestEveryCategoryInTheDataIsAccepted closes the loop the boot-derived
// vocabulary exists for: whatever the artifact carries must be a valid filter
// value.
//
// On its own this asserts only that two reads of the same artifact agree, which
// a hardcoded list satisfies just as well — TestVocabularyComesFromTheArtifact-
// NotTheBinary is what actually pins the design. This one still earns its place
// by covering every value rather than one, so a filter that accepted only part
// of the vocabulary would fail here.
func TestEveryCategoryInTheDataIsAccepted(t *testing.T) {
	var body struct {
		Data []categoryInfo `json:"data"`
	}
	if code := getAs(t, apiKey2, "/api/categories?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	for _, ci := range body.Data {
		var list eventList
		code := getAs(t, apiKey2, "/api/events?lang=ru&category="+ci.Category, &list)
		if code != http.StatusOK {
			t.Errorf("category %q exists in the data but the filter answered %d; the accepted "+
				"vocabulary and the stored values have diverged", ci.Category, code)
		}
	}
}

// TestCategoriesAreLanguageSpecific guards the same conflation the event
// endpoints are guarded against: the two artifacts are independent files.
func TestCategoriesAreLanguageSpecific(t *testing.T) {
	var ru, en struct {
		Data []categoryInfo `json:"data"`
	}
	getAs(t, apiKey2, "/api/categories?lang=ru", &ru)
	getAs(t, apiKey2, "/api/categories?lang=en", &en)

	if len(ru.Data) == 0 || len(en.Data) == 0 {
		t.Fatal("a language returned no categories")
	}
	// The RU fixture carries one extra event (id 4, category bitcoin) that the
	// EN fixture does not, so the two must differ.
	ruHas := map[string]bool{}
	for _, c := range ru.Data {
		ruHas[c.Category] = true
	}
	if !ruHas["bitcoin"] {
		t.Error("ru is missing the bitcoin category, which only its fixture carries")
	}
	for _, c := range en.Data {
		if c.Category == "bitcoin" {
			t.Error("en reports a bitcoin category; that row exists only in the ru fixture")
		}
	}
}

// TestHealthReportsTheVocabularyItServes is the assertion publish-db.sh depends
// on. Its verify step parses /health and would otherwise have no way to see that
// an artifact carries no categories: the boot log says so once, and nothing reads
// a boot log during a release.
//
// The count is checked against /api/categories rather than against a number
// written here. A literal would pass just as well against a field populated from
// a second query, or from a constant — what has to be true is that /health
// describes the vocabulary the service is actually validating against, and the
// endpoint is where that vocabulary is observable.
func TestHealthReportsTheVocabularyItServes(t *testing.T) {
	var health healthDoc
	fetchJSON(t, baseURL+"/health", &health)

	for _, lang := range []string{"en", "ru"} {
		t.Run(lang, func(t *testing.T) {
			db, found := health.Databases[lang]
			if !found {
				t.Fatalf("%s: absent from /health", lang)
			}
			if !db.Categories.Present {
				t.Fatalf("%s: /health reports no category column, but the fixture has one", lang)
			}

			var body struct {
				Data []categoryInfo `json:"data"`
			}
			if code := getAs(t, apiKey3, "/api/categories?lang="+lang, &body); code != http.StatusOK {
				t.Fatalf("/api/categories: want 200, got %d", code)
			}
			if len(body.Data) == 0 {
				t.Fatal("/api/categories returned nothing, so the count below proves nothing")
			}
			if db.Categories.Count != len(body.Data) {
				t.Errorf("%s: /health says %d categories, /api/categories lists %d — the "+
					"release check would be asserting against a number the service does not "+
					"filter by", lang, db.Categories.Count, len(body.Data))
			}
		})
	}
}

// TestEmptyVocabularyRejectsEveryCategory covers the artifact that has the
// column and carries no values in it.
//
// The filter used to accept every value in that state, on the reasoning that
// there was no vocabulary to validate against — so ?category=nonesuch answered
// 200 with an empty list, which is precisely what the 400 was added to prevent.
// That permissiveness was written for an artifact predating the column; that
// case is handled separately now, and this was the only route left to the silent
// empty result.
//
// Reproduced against a copy of a real artifact with `UPDATE events SET category
// = NULL` before it was written here. Upstream cannot publish such a file
// without breaking validator invariant 13, which is why nothing else would catch
// it: this is the API refusing to answer a question it cannot answer, not a
// guess about what the data will look like.
func TestEmptyVocabularyRejectsEveryCategory(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE events SET category = NULL`)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact whose category column is "+
			"empty. Every other endpoint answers correctly on it, exactly as on an artifact "+
			"predating the column: %v\n--- log ---\n%s", startErr, serviceLog)
	}
	// Distinguishes this from the missing-column path, which rejects categories
	// too — without it this test would pass against an artifact that simply had
	// no such column, i.e. for the wrong reason.
	if !strings.Contains(serviceLog, "no categories") {
		t.Errorf("the service degraded silently; nothing in its log says the column carries "+
			"no values:\n%s", serviceLog)
	}

	// The control: the column is there and the rest of the service is fine.
	var list eventList
	if code := getFrom(t, base, "/api/events?lang=ru&limit=100", &list); code != http.StatusOK {
		t.Fatalf("/api/events: want 200, got %d", code)
	}
	if list.Pagination.Total != 5 {
		t.Errorf("/api/events total: want 5, got %d", list.Pagination.Total)
	}

	// `bitcoin` is a real category in the unmutated fixture, and `nonesuch` never
	// was: with no vocabulary, both are equally unanswerable and both must be
	// refused.
	for _, category := range []string{"nonesuch", "bitcoin"} {
		var body struct {
			Error string `json:"error"`
		}
		code := getFrom(t, base, "/api/events?lang=ru&category="+category, &body)
		if code != http.StatusBadRequest {
			t.Errorf("category=%s against an empty vocabulary: want 400, got %d. 200 is an "+
				"empty list indistinguishable from a category that genuinely has no events",
				category, code)
		}
		if !strings.Contains(body.Error, "no categories") {
			t.Errorf("category=%s: the rejection does not say the artifact carries none: %q",
				category, body.Error)
		}
	}

	// And the discovery endpoint agrees, in the shape a client can iterate.
	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	if code := getFrom(t, base, "/api/categories?lang=ru", &raw); code != http.StatusOK {
		t.Fatalf("/api/categories: want 200, got %d", code)
	}
	if string(raw.Data) != "[]" {
		t.Errorf("/api/categories: want an empty array, got %s", raw.Data)
	}

	// This is the state publish-db.sh must refuse to publish, so /health has to
	// distinguish it from an artifact predating the column: the column is there,
	// and nothing is in it.
	var health healthDoc
	fetchJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("status: want ok — an empty vocabulary is not an index problem, and marking "+
			"it degraded would alarm for as long as such an artifact was served, got %q",
			health.Status)
	}
	for lang, db := range health.Databases {
		if !db.Categories.Present {
			t.Errorf("%s: /health says the column is absent; it is present and empty, which is "+
				"a different failure with a different fix", lang)
		}
		if db.Categories.Count != 0 {
			t.Errorf("%s: /health reports %d categories on an artifact where every value is NULL",
				lang, db.Categories.Count)
		}
	}
}

// TestServiceBootsWithoutACategoryColumn is a regression test for a boot failure
// that only an old artifact could trigger, which is precisely why nothing caught
// it: every fixture and every current release carries the column.
//
// `category` arrived on 2026-08-09. Every release before that has no such
// column, and those files are still on the box as rollback targets —
// publish-db.sh's rollback() re-points `current` at the previous release and
// restarts the service. Reading the vocabulary at boot made a missing column a
// fatal error, so this binary refused to start against any of them: a rollback
// became an outage, and a binary deploy silently required an artifact of at
// least a given age. Confirmed against a real superseded artifact — the pre-PR
// binary served it, this one exited with `no such column: category`.
//
// So the service must boot, keep answering everything that does not depend on
// the column, and refuse only the filter it genuinely cannot answer.
func TestServiceBootsWithoutACategoryColumn(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`ALTER TABLE events DROP COLUMN category`)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact predating the category "+
			"column. That artifact is a rollback target, and everything except ?category= "+
			"works perfectly well on it: %v\n--- log ---\n%s", startErr, serviceLog)
	}
	if !strings.Contains(serviceLog, "no category column") {
		t.Errorf("the service degraded silently; nothing in its log says the artifact has no "+
			"category column:\n%s", serviceLog)
	}

	// Everything not keyed on the column still works.
	var list eventList
	if code := getFrom(t, base, "/api/events?lang=ru&limit=100", &list); code != http.StatusOK {
		t.Fatalf("/api/events: want 200, got %d", code)
	}
	if list.Pagination.Total != 5 {
		t.Errorf("/api/events total: want 5, got %d", list.Pagination.Total)
	}

	// Search especially. It is the one handler that enumerates its columns by
	// hand instead of letting GORM derive them, so it is the one that names
	// e.category into a table that has none — and it answered 500 to every query
	// while the rest of the service was fine. Booting and then failing every
	// search is not a rollback that worked.
	var found eventList
	if code := getFrom(t, base, "/api/search?lang=ru&q=satoshi&limit=100", &found); code != http.StatusOK {
		t.Errorf("/api/search: want 200, got %d — the hand-written SELECT in "+
			"ftsSearchHandler still names a column this artifact does not have", code)
	}
	if len(found.Events) == 0 {
		t.Error("/api/search returned nothing for a term the fixture carries; a 200 with an " +
			"empty list is the failure this would otherwise hide")
	}

	// The filter cannot be answered, so it must be refused — not answered 200
	// with an empty list, which is the whole reason ?category= validates at all.
	var errBody struct {
		Error string `json:"error"`
	}
	if code := getFrom(t, base, "/api/events?lang=ru&category=bitcoin", &errBody); code != http.StatusBadRequest {
		t.Errorf("?category= against an artifact with no category column: want 400, got %d. "+
			"200 would be an empty list indistinguishable from a real one, and 500 would "+
			"be a server error for a question the server can answer", code)
	}
	if !strings.Contains(errBody.Error, "category") {
		t.Errorf("the rejection does not explain itself: %q", errBody.Error)
	}

	// And the discovery endpoint reports an empty list rather than failing, or
	// returning the JSON null a nil slice marshals to.
	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	if code := getFrom(t, base, "/api/categories?lang=ru", &raw); code != http.StatusOK {
		t.Fatalf("/api/categories: want 200, got %d", code)
	}
	if string(raw.Data) != "[]" {
		t.Errorf("/api/categories: want an empty array, got %s — null is what a nil slice "+
			"marshals to, and a caller has to special-case it", raw.Data)
	}

	// /health must say the column is absent rather than that the vocabulary is
	// empty. Serving this artifact is a correct outcome — it is what a rollback
	// looks like — and the release check treats the two differently.
	var health healthDoc
	fetchJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("status: want ok, got %q — a rollback target is not a degraded service",
			health.Status)
	}
	for lang, db := range health.Databases {
		if db.Categories.Present {
			t.Errorf("%s: /health claims a category column on an artifact that has none", lang)
		}
		if db.Categories.Count != 0 {
			t.Errorf("%s: /health reports %d categories with no column to hold them",
				lang, db.Categories.Count)
		}
	}
}
