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

type eventResponse struct {
	Data event `json:"data"`
}

func TestExactURLLookupUsesTheFullCanonicalPath(t *testing.T) {
	paths := []struct {
		path   string
		wantID int
	}{
		{"/2008-11-01/bitcoin-whitepaper-published/", 2},
		{"/2013-08-09/bitcoin-whitepaper-published/", 3},
	}

	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
			var body eventResponse
			code := get(t, "/api/events/url"+tc.path+"?lang=en", &body)
			if code != http.StatusOK {
				t.Fatalf("want 200, got %d", code)
			}
			if body.Data.ID != tc.wantID || body.Data.URLPath != tc.path {
				t.Errorf("got id=%d path=%q, want id=%d path=%q; lookup must match the full path",
					body.Data.ID, body.Data.URLPath, tc.wantID, tc.path)
			}
		})
	}
}

func TestExactURLLookupKeepsLanguagesIndependent(t *testing.T) {
	const path = "/2013-08-09/bitcoin-whitepaper-published/"
	var en, ru eventResponse
	if code := get(t, "/api/events/url"+path+"?lang=en", &en); code != http.StatusOK {
		t.Fatalf("en: want 200, got %d", code)
	}
	if code := get(t, "/api/events/url"+path+"?lang=ru", &ru); code != http.StatusOK {
		t.Fatalf("ru: want 200, got %d", code)
	}
	if en.Data.Title != "A duplicated tag lives here" {
		t.Errorf("en title: got %q", en.Data.Title)
	}
	if ru.Data.Title != "Повторяющийся slug в русской базе" {
		t.Errorf("ru title: got %q", ru.Data.Title)
	}
	if en.Data.URLPath != path || ru.Data.URLPath != path {
		t.Errorf("paths: en=%q ru=%q, want %q", en.Data.URLPath, ru.Data.URLPath, path)
	}
}

func TestExactURLLookupPreservesTheFullEventShape(t *testing.T) {
	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if code := get(t, "/api/events/url/2008-11-01/bitcoin-whitepaper-published/?lang=en", &body); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}

	for _, field := range []string{
		"id", "date", "title", "description", "tags", "media", "references",
		"url_path", "category", "landmark", "created_at", "updated_at",
	} {
		if _, ok := body.Data[field]; !ok {
			t.Errorf("response omitted %q", field)
		}
	}

	var got event
	raw, err := json.Marshal(body.Data)
	if err != nil {
		t.Fatalf("encoding event: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding event: %v", err)
	}
	if got.Date != "2008-11-01" {
		t.Errorf("date: got %q, want date-only 2008-11-01", got.Date)
	}
	if got.Category != "mustread" || !got.Landmark {
		t.Errorf("category/landmark: got %q/%v", got.Category, got.Landmark)
	}
	if got.Media == nil || *got.Media != `["https://example.org/whitepaper.png"]` {
		t.Errorf("media: got %v", got.Media)
	}
	if got.References == nil || *got.References != `["https://bitcoin.org/bitcoin.pdf"]` {
		t.Errorf("references: got %v", got.References)
	}
	if got.URLPath != "/2008-11-01/bitcoin-whitepaper-published/" {
		t.Errorf("url_path: got %q", got.URLPath)
	}

	var historical eventResponse
	if code := get(t, "/api/events/url/1881-09-29/birthday-of-ludwig-von-mises?lang=en", &historical); code != http.StatusOK {
		t.Fatalf("historical event: want 200, got %d", code)
	}
	if historical.Data.Date != "1881-09-29" || historical.Data.Media != nil || historical.Data.References != nil {
		t.Errorf("historical event lost date/null semantics: %#v", historical.Data)
	}
}

func TestExactURLLookupNormalizesOuterSlashes(t *testing.T) {
	for _, path := range []string{
		"/api/events/url/2013-08-09/bitcoin-whitepaper-published",
		"/api/events/url/2013-08-09/bitcoin-whitepaper-published/",
		"/api/events/url//2013-08-09/bitcoin-whitepaper-published",
		"/api/events/url//2013-08-09/bitcoin-whitepaper-published/",
	} {
		t.Run(path, func(t *testing.T) {
			var body eventResponse
			if code := get(t, path+"?lang=en", &body); code != http.StatusOK {
				t.Fatalf("want 200, got %d", code)
			}
			if body.Data.ID != 3 {
				t.Errorf("got event %d, want 3", body.Data.ID)
			}
		})
	}
}

func TestExactURLLookupRejectsMalformedPaths(t *testing.T) {
	bad := []string{
		"/api/events/url",
		"/api/events/url/",
		"/api/events/url/bitcoin-whitepaper-published",
		"/api/events/url/2023-02-29/bitcoin-whitepaper-published",
		"/api/events/url/2013-08-09",
		"/api/events/url/2013-08-09/",
		"/api/events/url/2013-08-09/bitcoin-whitepaper-published/extra",
		"/api/events/url/2013-08-09//bitcoin-whitepaper-published",
		"/api/events/url/bitcoin-whitepaper-published/2013-08-09",
	}

	for _, path := range bad {
		t.Run(path, func(t *testing.T) {
			var body map[string]interface{}
			if code := get(t, path+"?lang=en", &body); code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", code)
			}
			if msg, _ := body["error"].(string); !strings.Contains(strings.ToLower(msg), "path") {
				t.Errorf("unhelpful error %q", msg)
			}
		})
	}
}

func TestExactURLLookupDistinguishesMissingFromMalformed(t *testing.T) {
	var missing map[string]interface{}
	if code := get(t, "/api/events/url/2013-08-09/not-in-the-fixture?lang=en", &missing); code != http.StatusNotFound {
		t.Fatalf("valid missing path: want 404, got %d", code)
	}
	if msg, _ := missing["error"].(string); msg == "" {
		t.Error("404 has no public error message")
	}
}

func TestExactURLLookupRequiresAuthenticationAndDoesNotShadowIDs(t *testing.T) {
	if code := request(t, http.MethodGet,
		"/api/events/url/2008-11-01/bitcoin-whitepaper-published/", false, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated exact lookup: want 401, got %d", code)
	}

	var exact eventResponse
	if code := get(t, "/api/events/url/2008-11-01/bitcoin-whitepaper-published/?lang=en", &exact); code != http.StatusOK {
		t.Fatalf("wildcard route reached the ID parser: got %d, want 200", code)
	}
	var byID eventResponse
	if code := get(t, "/api/events/2?lang=en", &byID); code != http.StatusOK {
		t.Fatalf("existing ID route: want 200, got %d", code)
	}
	if exact.Data.ID != byID.Data.ID {
		t.Errorf("exact lookup returned %d, ID route returned %d", exact.Data.ID, byID.Data.ID)
	}
}

func TestExactURLLookupDoesNotTurnDatabaseFailureIntoNotFound(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		if _, err := db.Exec(`DROP INDEX idx_events_url_path`); err != nil {
			return err
		}
		_, err := db.Exec(`ALTER TABLE events DROP COLUMN url_path`)
		return err
	})
	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("service did not boot against the failure fixture: %v\n--- log ---\n%s", startErr, serviceLog)
	}

	var body map[string]interface{}
	code := getFrom(t, base, "/api/events/url/2008-11-01/bitcoin-whitepaper-published/?lang=en", &body)
	if code != http.StatusInternalServerError {
		t.Fatalf("database failure: want 500, got %d", code)
	}
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatal("500 has no public error message")
	}
	for _, detail := range []string{"no such column", "sqlite", "url_path"} {
		if strings.Contains(strings.ToLower(msg), detail) {
			t.Errorf("public error leaks database detail %q: %q", detail, msg)
		}
	}

	// The fixture is isolated and remains a read-only deployment-shaped pair.
	for _, lang := range []string{"en", "ru"} {
		info, err := os.Stat(filepath.Join(dir, "events_"+lang+".db"))
		if err != nil {
			t.Fatalf("stat %s: %v", lang, err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Errorf("%s fixture mode: got %o, want 444", lang, info.Mode().Perm())
		}
	}
}
