package tests

import (
	"net/http"
	"net/url"
	"testing"
)

type categoryInfo struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// TestCategoryFilter covers the filter's happy path against the fixture, whose
// categories are deliberately not equal to any row's first tag — a filter that
// accidentally matched on tags would pass a fixture where the two agreed.
func TestCategoryFilter(t *testing.T) {
	// Fixture categories, RU: holiday(1), mustread(1), archives(1), first(1),
	// bitcoin(1). `bitcoin` is the important one: it is a category carried by a
	// row whose tags do not include it, which is the exact shape canonical has
	// after the tag was retired.
	for _, tc := range []struct {
		category string
		want     int
	}{
		{"holiday", 1},
		{"archives", 1},
		{"bitcoin", 1},
		{"first", 1},
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
			// they cannot see.
			if body.Error == "" {
				t.Error("a rejected category must explain itself")
			}
		})
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

// TestEveryCategoryInTheDataIsAccepted closes the loop the boot-derived
// vocabulary exists for: whatever the artifact carries must be a valid filter
// value. A hardcoded list would drift from the data and fail exactly here —
// which is what would have happened on 2026-08-10, when canonical added
// `security` a day after the column shipped.
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
