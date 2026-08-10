package tests

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServesReadOnlyArtifact is the regression that fails on the box rather
// than on a laptop: the service must boot against a 0444 database inside a 0555
// directory. Anything issuing DDL or negotiating WAL at startup dies here.
// TestMain has already proved the boot; this checks it is actually serving.
func TestServesReadOnlyArtifact(t *testing.T) {
	var health healthDoc
	if code := request(t, http.MethodGet, "/health", false, &health); code != http.StatusOK {
		t.Fatalf("/health: want 200 without an API key, got %d", code)
	}

	if health.Status != "ok" {
		t.Errorf("status: want ok, got %q", health.Status)
	}
	for _, lang := range []string{"en", "ru"} {
		db, ok := health.Databases[lang]
		if !ok {
			t.Fatalf("/health is missing the %q database", lang)
		}
		want := fixtureSums["events_"+lang+".db"]
		if db.SHA256 != want {
			t.Errorf("%s sha256: want %s, got %s", lang, want, db.SHA256)
		}
		if db.Rows == 0 {
			t.Errorf("%s: reports 0 rows", lang)
		}
		// The release check reads these: they are what turns "the service is
		// up" into "the service is serving the artifact I just published, and
		// all of it is searchable".
		if db.FTS.Indexed != db.Rows {
			t.Errorf("%s fts.indexed: want %d to match rows, got %d", lang, db.Rows, db.FTS.Indexed)
		}
		if !db.FTS.Consistent {
			t.Errorf("%s fts.consistent: want true", lang)
		}
	}
	if health.Databases["ru"].Rows != 5 || health.Databases["en"].Rows != 4 {
		t.Errorf("rows: want ru=5 en=4, got ru=%d en=%d",
			health.Databases["ru"].Rows, health.Databases["en"].Rows)
	}
}

// TestArtifactIsNeverWritten is the whole premise. A read-write open rewrites
// the header even with no UPDATE, and leaves sidecars behind.
func TestArtifactIsNeverWritten(t *testing.T) {
	for name, want := range fixtureSums {
		got, err := sha256File(filepath.Join(artifactDir, name))
		if err != nil {
			t.Fatalf("hashing %s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s changed while the service was running:\n  was %s\n  now %s", name, want, got)
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		matches, _ := filepath.Glob(filepath.Join(artifactDir, "*"+suffix))
		for _, m := range matches {
			t.Errorf("the service created %s; the open was not read-only", filepath.Base(m))
		}
	}
	entries, err := os.ReadDir(artifactDir)
	if err != nil {
		t.Fatalf("reading artifact dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("artifact directory holds %d files, want 2", len(entries))
	}
}

// TestEventContract pins the four fields the Telegram bot reads, plus the two
// null rules. Every assertion here is a change made deliberately, and each one
// is invisible to a test that only checks the status code.
func TestEventContract(t *testing.T) {
	var list eventList
	if code := get(t, "/api/events?month=9&day=29&limit=100&lang=ru", &list); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if len(list.Events) != 1 {
		t.Fatalf("want 1 event for 29 September, got %d", len(list.Events))
	}
	e := list.Events[0]

	// A plain date, not an RFC 3339 timestamp with an invented time and zone.
	// The driver hands back a time.Time here because the column is declared
	// `date`, so this passing means DateString.Scan is doing its job.
	if e.Date != "1881-09-29" {
		t.Errorf("date: want %q, got %q", "1881-09-29", e.Date)
	}
	// Launch-critical: the bot builds its link back to the site from this.
	if e.URLPath != "/1881-09-29/birthday-of-ludwig-von-mises/" {
		t.Errorf("url_path: want the event's path, got %q", e.URLPath)
	}
	// The single mandatory classification, and what the website colours and
	// filters by. Pinned to a value that is not this row's first tag, because
	// consumers used to derive category from tags[0] and that inference is now
	// wrong — a fixture where the two agreed would keep it looking correct.
	if e.Category != "holiday" {
		t.Errorf("category: want %q, got %q", "holiday", e.Category)
	}
	// Absence is null and only null — never "" and never "[]".
	if e.Media != nil {
		t.Errorf("media: want null, got %q", *e.Media)
	}
	if e.References != nil {
		t.Errorf("references: want null, got %q", *e.References)
	}
	// Absent timestamps must not render as the year-1 zero time.
	if e.CreatedAt != nil {
		t.Errorf("created_at: want null, got %q", *e.CreatedAt)
	}
	if e.UpdatedAt != nil {
		t.Errorf("updated_at: want null, got %q", *e.UpdatedAt)
	}
}

// TestPresentValuesStillRender is the other half of the null contract: a
// pointer field must not turn a real value into null.
func TestPresentValuesStillRender(t *testing.T) {
	var single struct {
		Data event `json:"data"`
	}
	if code := get(t, "/api/events/2?lang=ru", &single); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	e := single.Data
	if e.Media == nil || *e.Media == "" {
		t.Errorf("media: want the stored JSON array, got %v", e.Media)
	}
	if e.References == nil {
		t.Errorf("references: want the stored JSON array, got null")
	}
	if e.CreatedAt == nil {
		t.Errorf("created_at: want a timestamp, got null")
	}
	if e.Date != "2008-11-01" {
		t.Errorf("date: want 2008-11-01, got %q", e.Date)
	}
}

// TestLanguagesAreSeparate guards against the two databases being conflated.
// Ids are independent per language, so id is not a cross-language key.
func TestLanguagesAreSeparate(t *testing.T) {
	var ru, en eventList
	get(t, "/api/events?limit=100&lang=ru", &ru)
	get(t, "/api/events?limit=100&lang=en", &en)

	if ru.Pagination.Total != 5 {
		t.Errorf("ru total: want 5, got %d", ru.Pagination.Total)
	}
	if en.Pagination.Total != 4 {
		t.Errorf("en total: want 4, got %d", en.Pagination.Total)
	}
}

// TestSearchWorks proves two things at once that are easy to confuse: that
// `references` is quoted in the SELECT — unquoted it is a reserved word and
// every search returns 500 — and that the binary was built with -tags fts5,
// without which events_fts does not exist at all.
func TestSearchWorks(t *testing.T) {
	var list eventList
	if code := get(t, "/api/search?q=bitcoin&lang=ru", &list); code != http.StatusOK {
		t.Fatalf("/api/search: want 200, got %d — check e.\"references\" quoting and -tags fts5", code)
	}
	if list.Pagination.Total == 0 {
		t.Fatal("/api/search matched nothing; the FTS index is not being read")
	}
	for _, e := range list.Events {
		if e.URLPath == "" {
			t.Errorf("event %d: search results omit url_path", e.ID)
		}
		if e.Date == "" || len(e.Date) != len("2006-01-02") {
			t.Errorf("event %d: search results carry a malformed date %q", e.ID, e.Date)
		}
	}
}

// TestTagsEndpointResponds is the hang regression. /api/tags never returned —
// not slowly, never — because a comment sat after the query's final semicolon.
// The request timeout would now cap it either way, so this asserts a real
// answer rather than merely a prompt one.
func TestTagsEndpointResponds(t *testing.T) {
	var body struct {
		Data []tagInfo `json:"data"`
	}
	if code := get(t, "/api/tags?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("/api/tags: want 200, got %d", code)
	}
	if len(body.Data) == 0 {
		t.Fatal("/api/tags returned no tags")
	}
}

// TestTagCountsAgreeWithByTagEndpoint pins the two endpoints together. They
// disagreed by one for 'satoshi' because /api/tags counted occurrences while
// /api/events/tags/:tag counts events, and some events list a tag twice.
func TestTagCountsAgreeWithByTagEndpoint(t *testing.T) {
	var body struct {
		Data []tagInfo `json:"data"`
	}
	get(t, "/api/tags?lang=ru", &body)

	for _, tag := range body.Data {
		var list eventList
		if code := get(t, "/api/events/tags/"+tag.Tag+"?lang=ru&limit=1", &list); code != http.StatusOK {
			t.Fatalf("/api/events/tags/%s: want 200, got %d", tag.Tag, code)
		}
		if list.Pagination.Total != tag.Count {
			t.Errorf("tag %q: /api/tags says %d, /api/events/tags says %d",
				tag.Tag, tag.Count, list.Pagination.Total)
		}
	}
}

// TestTagCountsEventsNotOccurrences is the specific case behind that
// disagreement: fixture event 3 lists "satoshi" twice.
func TestTagCountsEventsNotOccurrences(t *testing.T) {
	var body struct {
		Data []tagInfo `json:"data"`
	}
	get(t, "/api/tags?lang=ru", &body)

	for _, tag := range body.Data {
		if tag.Tag != "satoshi" {
			continue
		}
		// Events 2, 3 and 4 carry it; event 3 carries it twice.
		if tag.Count != 3 {
			t.Fatalf("satoshi: want 3 events, got %d — counting occurrences, not events", tag.Count)
		}
		return
	}
	t.Fatal("satoshi missing from /api/tags")
}

// TestTagFilterIgnoresLikeWildcards guards a defect that answered a question
// nobody asked: the tag went into a LIKE pattern, so /api/events/tags/%
// returned every event and /api/events/tags/_____ every five-letter tag.
//
// Each probe is paired with the literal tag it would match under LIKE, and the
// literal is asserted to match something. Without that pairing the test passes
// whenever the probe matches nothing for any reason at all — which is exactly
// how an earlier version of it passed while the bug was present: no fixture tag
// was five characters long, and Fiber does not percent-decode a route
// parameter, so neither `_____` nor `%25` could match even under LIKE.
func TestTagFilterIgnoresLikeWildcards(t *testing.T) {
	total := func(tag string) int {
		t.Helper()
		var list eventList
		if code := get(t, "/api/events/tags/"+tag+"?lang=ru&limit=1", &list); code != http.StatusOK {
			t.Fatalf("tag %q: want 200, got %d", tag, code)
		}
		return list.Pagination.Total
	}

	// Both probes reach the handler as written. A percent-encoded `%` is
	// deliberately not among them: Fiber hands the route parameter over still
	// encoded, so `%25` arrives as the literal three characters and can never
	// match anything, bug or no bug. An assertion that cannot fail is worse
	// than no assertion, because it reads like coverage.
	probes := []struct{ wildcard, literal string }{
		{"satosh_", "satoshi"}, // _ matches exactly one character
		{"_____", "price"},     // five characters, and `price` is five long
	}

	for _, p := range probes {
		// The control: if the literal matches nothing then the probe proves
		// nothing either, and the test would be measuring its own fixture.
		if n := total(p.literal); n == 0 {
			t.Fatalf("control tag %q matches no events; the %q probe would pass vacuously",
				p.literal, p.wildcard)
		}
		if n := total(p.wildcard); n != 0 {
			t.Errorf("tag %q matched %d events; LIKE wildcards are being honoured",
				p.wildcard, n)
		}
	}
}

// TestTagFilterIsCaseInsensitive keeps the documented behaviour while fixing
// the wildcards.
func TestTagFilterIsCaseInsensitive(t *testing.T) {
	var lower, upper eventList
	get(t, "/api/events/tags/satoshi?lang=ru&limit=1", &lower)
	get(t, "/api/events/tags/SATOSHI?lang=ru&limit=1", &upper)

	if lower.Pagination.Total == 0 {
		t.Fatal("satoshi matched nothing")
	}
	if lower.Pagination.Total != upper.Pagination.Total {
		t.Errorf("case sensitivity: satoshi=%d SATOSHI=%d",
			lower.Pagination.Total, upper.Pagination.Total)
	}
}

// TestNoWriteEndpoints holds the line the whole rework is about.
//
// The expected code is asserted exactly, not merely "not a success". A
// re-added handler called with an empty body answers 400, so a test that only
// rejected 2xx would let the write half back in without a word. 405 means the
// path exists for other methods and this one is not routed; 404 means no such
// path at all.
func TestNoWriteEndpoints(t *testing.T) {
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/events", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/events/1", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/events/1", http.StatusMethodNotAllowed},
		// 405 rather than 404: /api/events/:id makes `batch` a valid path for
		// GET, so the router reports the method as unrouted, not the path as
		// missing.
		{http.MethodPost, "/api/events/batch", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/migrate", http.StatusNotFound},
	}
	for _, c := range cases {
		if code := request(t, c.method, c.path, true, nil); code != c.want {
			t.Errorf("%s %s: want %d, got %d — a write endpoint may be registered again",
				c.method, c.path, c.want, code)
		}
	}
}

// TestDeletedRoutesAreGone covers the two stubs that returned HTTP 200 with the
// body "Not Implemented", which a consumer cannot tell from a real answer.
func TestDeletedRoutesAreGone(t *testing.T) {
	for _, path := range []string{"/api/events/date/2013-08-09", "/api/events/month/8"} {
		if code := request(t, http.MethodGet, path, true, nil); code != http.StatusNotFound {
			t.Errorf("GET %s: want 404, got %d", path, code)
		}
	}
}

// TestAuthenticationRequired — /api needs a key, /health must not.
func TestAuthenticationRequired(t *testing.T) {
	if code := request(t, http.MethodGet, "/api/events?limit=1", false, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /api/events: want 401, got %d", code)
	}
	if code := request(t, http.MethodGet, "/health", false, nil); code != http.StatusOK {
		t.Errorf("unauthenticated /health: want 200, got %d — a deploy check that needs a secret gets skipped", code)
	}
}

// TestCORS keeps the preflight contract, and checks that the advertised
// methods no longer include the writes that were removed.
func TestCORS(t *testing.T) {
	preflight := func(origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodOptions, baseURL+"/api/events", nil)
		if err != nil {
			t.Fatalf("building preflight: %v", err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)

		res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("preflight from %s: %v", origin, err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	denied := preflight("http://evil.example")
	if h := denied.Header.Get("Access-Control-Allow-Origin"); h != "" {
		t.Errorf("disallowed origin got Access-Control-Allow-Origin: %q", h)
	}

	allowed := preflight(allowedOrigin)
	if allowed.StatusCode != http.StatusNoContent {
		t.Errorf("allowed preflight: want 204, got %d", allowed.StatusCode)
	}
	if h := allowed.Header.Get("Access-Control-Allow-Origin"); h != allowedOrigin {
		t.Errorf("Access-Control-Allow-Origin: want %q, got %q", allowedOrigin, h)
	}

	methods := allowed.Header.Get("Access-Control-Allow-Methods")
	for _, write := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if strings.Contains(methods, write) {
			t.Errorf("Access-Control-Allow-Methods still advertises %s: %q", write, methods)
		}
	}
}
