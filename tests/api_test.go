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
	var health struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		Databases map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Rows   int64  `json:"rows"`
		} `json:"databases"`
	}
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
	}
	if health.Databases["ru"].Rows != 4 || health.Databases["en"].Rows != 3 {
		t.Errorf("rows: want ru=4 en=3, got ru=%d en=%d",
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

	if ru.Pagination.Total != 4 {
		t.Errorf("ru total: want 4, got %d", ru.Pagination.Total)
	}
	if en.Pagination.Total != 3 {
		t.Errorf("en total: want 3, got %d", en.Pagination.Total)
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
// disagreed by one for 'bitcoin' because /api/tags counted occurrences while
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
// disagreement: fixture event 3 lists "bitcoin" twice.
func TestTagCountsEventsNotOccurrences(t *testing.T) {
	var body struct {
		Data []tagInfo `json:"data"`
	}
	get(t, "/api/tags?lang=ru", &body)

	for _, tag := range body.Data {
		if tag.Tag != "bitcoin" {
			continue
		}
		// Events 2, 3 and 4 carry it; event 3 carries it twice.
		if tag.Count != 3 {
			t.Fatalf("bitcoin: want 3 events, got %d — counting occurrences, not events", tag.Count)
		}
		return
	}
	t.Fatal("bitcoin missing from /api/tags")
}

// TestTagFilterIgnoresLikeWildcards guards a defect that answered a question
// nobody asked: the tag went into a LIKE pattern, so /api/events/tags/%
// returned every event and /api/events/tags/_____ every five-letter tag.
func TestTagFilterIgnoresLikeWildcards(t *testing.T) {
	for _, tag := range []string{"%25", "_____", "%25bitcoin%25"} {
		var list eventList
		if code := get(t, "/api/events/tags/"+tag+"?lang=ru&limit=1", &list); code != http.StatusOK {
			t.Fatalf("tag %q: want 200, got %d", tag, code)
		}
		if list.Pagination.Total != 0 {
			t.Errorf("tag %q matched %d events; LIKE wildcards are being honoured",
				tag, list.Pagination.Total)
		}
	}
}

// TestTagFilterIsCaseInsensitive keeps the documented behaviour while fixing
// the wildcards.
func TestTagFilterIsCaseInsensitive(t *testing.T) {
	var lower, upper eventList
	get(t, "/api/events/tags/bitcoin?lang=ru&limit=1", &lower)
	get(t, "/api/events/tags/BITCOIN?lang=ru&limit=1", &upper)

	if lower.Pagination.Total == 0 {
		t.Fatal("bitcoin matched nothing")
	}
	if lower.Pagination.Total != upper.Pagination.Total {
		t.Errorf("case sensitivity: bitcoin=%d BITCOIN=%d",
			lower.Pagination.Total, upper.Pagination.Total)
	}
}

// TestNoWriteEndpoints holds the line the whole rework is about.
func TestNoWriteEndpoints(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/events"},
		{http.MethodPut, "/api/events/1"},
		{http.MethodDelete, "/api/events/1"},
		{http.MethodPost, "/api/events/batch"},
		{http.MethodPost, "/api/migrate"},
	}
	for _, c := range cases {
		code := request(t, c.method, c.path, true, nil)
		if code == http.StatusOK || code == http.StatusCreated || code == http.StatusNoContent {
			t.Errorf("%s %s: answered %d — a write endpoint is still registered", c.method, c.path, code)
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
