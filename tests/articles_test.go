package tests

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type articleTranslation struct {
	Locale string `json:"locale"`
	Slug   string `json:"slug"`
}

type articleDetail struct {
	ArticleKey    string               `json:"article_key"`
	Slug          string               `json:"slug"`
	Title         string               `json:"title"`
	Body          string               `json:"body_markdown"`
	Sources       []string             `json:"sources"`
	PublishedAt   *string              `json:"published_at"`
	Metadata      json.RawMessage      `json:"metadata"`
	RelatedEvents []event              `json:"related_events"`
	Translations  []articleTranslation `json:"translations"`
}

type articleDetailBody struct {
	Data articleDetail `json:"data"`
}

var articleSummaryKeys = []string{"article_key", "slug", "title", "sources", "created_at", "updated_at", "published_at"}

const articleMetadata = `{"title":"История","description":"A curated history.","image":{"url":"https://media.21ideas.org/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.jpg","alt":"Room77 entrance","width":999,"height":537,"credit":"Photo credit"}}`

func seedArticles(db *sql.DB, lang string) error {
	if _, err := db.Exec(`CREATE TABLE articles (
 article_key TEXT PRIMARY KEY NOT NULL CHECK(length(trim(article_key)) > 0),
 slug TEXT UNIQUE NOT NULL CHECK(length(trim(slug)) > 0),
 title TEXT NOT NULL CHECK(length(trim(title)) > 0),
 body_markdown TEXT NOT NULL CHECK(length(trim(body_markdown)) > 0),
 sources TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(sources) AND json_type(sources) = 'array'),
 status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft','published')),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 published_at TEXT,
 CHECK(status != 'published' OR published_at IS NOT NULL)
);
ALTER TABLE articles ADD COLUMN metadata TEXT CHECK(metadata IS NULL OR (json_valid(metadata) AND json_type(metadata) = 'object'));
CREATE TABLE event_articles (
 event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE RESTRICT,
 article_key TEXT NOT NULL REFERENCES articles(article_key) ON DELETE RESTRICT,
 is_primary INTEGER NOT NULL DEFAULT 0 CHECK(is_primary IN (0,1)),
 sort_order INTEGER NOT NULL DEFAULT 0 CHECK(sort_order >= 0),
 PRIMARY KEY(event_id, article_key)
);
CREATE UNIQUE INDEX idx_event_articles_primary ON event_articles(event_id) WHERE is_primary = 1;
CREATE INDEX idx_event_articles_article ON event_articles(article_key, event_id);
`); err != nil {
		return err
	}
	if lang != "ru" {
		return nil
	}
	if _, err := db.Exec(`INSERT INTO articles(article_key,slug,title,body_markdown,sources,status,created_at,updated_at,published_at) VALUES
 ('fixture-history','history','History','Verified **history**.','["https://source.example/history"]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z'),
 ('fixture-draft','draft','Secret draft','Never expose me','[]','draft','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z',NULL);
 INSERT INTO event_articles(event_id,article_key,is_primary,sort_order) VALUES(5,'fixture-history',0,0),(3,'fixture-history',0,0),(3,'fixture-draft',1,0),(2,'fixture-history',1,0),(1,'fixture-history',1,0);`); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE articles SET metadata=? WHERE article_key='fixture-history'`, articleMetadata)
	return err
}

func compareArticleSchema(t *testing.T, canonical string) {
	t.Helper()
	if err := articleSchemaMismatch(canonical, filepath.Join(artifactDir, "events_ru.db")); err != nil {
		t.Error(err)
	}
}

func articleSchemaMismatch(canonical, fixture string) error {
	source, err := sql.Open("sqlite3", "file:"+canonical+"?mode=ro")
	if err != nil {
		return err
	}
	defer source.Close()
	tested, err := sql.Open("sqlite3", "file:"+fixture+"?mode=ro")
	if err != nil {
		return err
	}
	defer tested.Close()
	var present int
	if err := source.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('articles','event_articles') AND type='table'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	if present != 2 {
		return errors.New("canonical carries a partial article schema")
	}
	var mismatches []error
	for _, name := range []string{"articles", "event_articles", "idx_event_articles_primary", "idx_event_articles_article"} {
		var canonicalSQL, fixtureSQL string
		if err := tested.QueryRow("SELECT sql FROM sqlite_master WHERE name=?", name).Scan(&fixtureSQL); err != nil {
			return fmt.Errorf("fixture %s: %w", name, err)
		}
		if err := source.QueryRow("SELECT sql FROM sqlite_master WHERE name=?", name).Scan(&canonicalSQL); err != nil {
			mismatches = append(mismatches, fmt.Errorf("canonical %s: %w", name, err))
			continue
		}
		accepted := []string{fixtureSQL}
		if name == "articles" {
			legacy, err := legacyArticlesSQL(fixtureSQL)
			if err != nil {
				return fmt.Errorf("deriving legacy articles schema: %w", err)
			}
			accepted = append(accepted, legacy)
		}
		matched := false
		for _, want := range accepted {
			if normalizeSchemaSQL(canonicalSQL) == normalizeSchemaSQL(want) {
				matched = true
			}
		}
		if !matched {
			mismatches = append(mismatches, fmt.Errorf("%s: canonical article schema differs from tested fixture", name))
		}
	}
	return errors.Join(mismatches...)
}

const schemaSQLSeparators = "(),;\n"

func normalizeSchemaSQL(statement string) string {
	var normalized strings.Builder
	var closing, previous rune
	spaced := false
	for _, char := range statement {
		switch {
		case closing != 0:
			if char == closing {
				closing = 0
			}
		case strings.ContainsRune(" \t\n\f\r", char):
			spaced = true
			continue
		default:
			if spaced && previous != 0 && !strings.ContainsRune(schemaSQLSeparators, previous) && !strings.ContainsRune(schemaSQLSeparators, char) {
				normalized.WriteRune(' ')
			}
			switch {
			case char == '\'' || char == '"' || char == '`':
				closing = char
			case char == '[':
				closing = ']'
			case char == '-' && previous == '-' && !spaced:
				closing = '\n'
			}
			spaced = false
		}
		normalized.WriteRune(char)
		previous = char
	}
	return normalized.String()
}

func legacyArticlesSQL(articlesSQL string) (string, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(articlesSQL); err != nil {
		return "", err
	}
	if _, err := db.Exec("ALTER TABLE articles DROP COLUMN metadata"); err != nil {
		return "", err
	}
	var legacy string
	err = db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='articles'").Scan(&legacy)
	return legacy, err
}

type articleRewrite struct{ name, old, new string }

func syntheticCanonical(t *testing.T, skip []string, rewrites []articleRewrite, extra string) string {
	t.Helper()
	fixture, err := sql.Open("sqlite3", "file:"+filepath.Join(artifactDir, "events_ru.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	rows, err := fixture.Query(`SELECT name, sql FROM sqlite_master WHERE tbl_name IN ('articles','event_articles') AND sql IS NOT NULL ORDER BY type DESC, name`)
	if err != nil {
		t.Fatal(err)
	}
	var names, statements []string
	for rows.Next() {
		var name, stmt string
		if err := rows.Scan(&name, &stmt); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		statements = append(statements, stmt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	path := filepath.Join(t.TempDir(), "canonical.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE events (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	skipped := set(skip)
	applied := 0
	for i, name := range names {
		if skipped[name] {
			continue
		}
		stmt := statements[i]
		for _, r := range rewrites {
			if r.name != name {
				continue
			}
			if !strings.Contains(stmt, r.old) {
				t.Fatalf("%s has no %q to rewrite: %s", name, r.old, stmt)
			}
			stmt = strings.Replace(stmt, r.old, r.new, 1)
			applied++
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if applied != len(rewrites) {
		t.Fatalf("applied %d of %d rewrites", applied, len(rewrites))
	}
	if extra != "" {
		if _, err := db.Exec(extra); err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
	}
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArticleSchemaGuard(t *testing.T) {
	const metadataColumn = "metadata TEXT CHECK(metadata IS NULL OR (json_valid(metadata) AND json_type(metadata) = 'object'))"
	const statusCheck = "CHECK(status IN ('draft','published'))"
	const spacedStatusCheck = "CHECK ( status\n\tIN ( 'draft' , 'published' ) )"
	relations := []string{"event_articles", "idx_event_articles_primary", "idx_event_articles_article"}
	for _, tc := range []struct {
		name     string
		skip     []string
		rewrites []articleRewrite
		extra    string
		ok       bool
	}{
		{name: "metadata column", ok: true},
		{name: "legacy metadata dropped", extra: "ALTER TABLE articles DROP COLUMN metadata", ok: true},
		{name: "legacy created without metadata", rewrites: []articleRewrite{{"articles", ", " + metadataColumn, ""}}, ok: true},
		{name: "no article tables", skip: append([]string{"articles"}, relations...), ok: true},
		{name: "relations table missing", skip: relations},
		{name: "articles table missing", skip: []string{"articles"}},
		{name: "required column missing", extra: "ALTER TABLE articles DROP COLUMN sources"},
		{name: "legacy required column missing", extra: "ALTER TABLE articles DROP COLUMN metadata; ALTER TABLE articles DROP COLUMN sources"},
		{name: "required column type", rewrites: []articleRewrite{{"articles", "title TEXT NOT NULL", "title BLOB NOT NULL"}}},
		{name: "required constraint", rewrites: []articleRewrite{{"articles", "CHECK(status != 'published' OR published_at IS NOT NULL)", "CHECK(status != 'published')"}}},
		{name: "unknown column", extra: "ALTER TABLE articles ADD COLUMN summary TEXT"},
		{name: "legacy unknown column", extra: "ALTER TABLE articles DROP COLUMN metadata; ALTER TABLE articles ADD COLUMN summary TEXT"},
		{name: "metadata type", rewrites: []articleRewrite{{"articles", "metadata TEXT CHECK", "metadata BLOB CHECK"}}},
		{name: "metadata not null", rewrites: []articleRewrite{{"articles", "metadata TEXT CHECK", "metadata TEXT NOT NULL CHECK"}}},
		{name: "metadata constraint changed", rewrites: []articleRewrite{{"articles", "json_type(metadata) = 'object'", "json_type(metadata) = 'array'"}}},
		{name: "metadata constraint missing", rewrites: []articleRewrite{{"articles", metadataColumn, "metadata TEXT"}}},
		{name: "relation foreign key", rewrites: []articleRewrite{{"event_articles", "REFERENCES articles(article_key) ON DELETE RESTRICT", "REFERENCES articles(article_key) ON DELETE CASCADE"}}},
		{name: "relation primary key", rewrites: []articleRewrite{{"event_articles", "PRIMARY KEY(event_id, article_key)", "PRIMARY KEY(event_id, article_key, sort_order)"}}},
		{name: "relation unknown column", extra: "ALTER TABLE event_articles ADD COLUMN note TEXT"},
		{name: "primary index missing", skip: []string{"idx_event_articles_primary"}},
		{name: "primary index not partial", rewrites: []articleRewrite{{"idx_event_articles_primary", " WHERE is_primary = 1", ""}}},
		{name: "article index changed", rewrites: []articleRewrite{{"idx_event_articles_article", "(article_key, event_id)", "(event_id, article_key)"}}},
		{name: "article index missing", skip: []string{"idx_event_articles_article"}},
		{name: "external whitespace", rewrites: []articleRewrite{{"articles", statusCheck, spacedStatusCheck}, {"idx_event_articles_article", "(article_key, event_id)", "(  article_key ,event_id\n)"}}, ok: true},
		{name: "legacy external whitespace", rewrites: []articleRewrite{{"articles", ", " + metadataColumn, ""}, {"articles", statusCheck, spacedStatusCheck}}, ok: true},
		{name: "status literal inner space", rewrites: []articleRewrite{{"articles", statusCheck, "CHECK(status IN ('draft','pub lished'))"}}},
		{name: "status literal double space", rewrites: []articleRewrite{{"articles", statusCheck, "CHECK(status IN ('draft','pub  lished'))"}}},
		{name: "status literal leading space", rewrites: []articleRewrite{{"articles", statusCheck, "CHECK(status IN ('draft',' published'))"}}},
		{name: "status literal trailing space", rewrites: []articleRewrite{{"articles", statusCheck, "CHECK(status IN ('draft','published '))"}}},
		{name: "default literal space", rewrites: []articleRewrite{{"articles", "DEFAULT 'draft'", "DEFAULT 'dr aft'"}}},
		{name: "metadata literal space", rewrites: []articleRewrite{{"articles", "json_type(metadata) = 'object'", "json_type(metadata) = 'ob ject'"}}},
		{name: "required column type merged into name", rewrites: []articleRewrite{{"articles", "created_at TEXT NOT NULL", "created_atTEXT NOT NULL"}}},
		{name: "legacy required column type merged into name", rewrites: []articleRewrite{{"articles", ", " + metadataColumn, ""}, {"articles", "created_at TEXT NOT NULL", "created_atTEXT NOT NULL"}}},
		{name: "required column name with non-breaking space", rewrites: []articleRewrite{{"articles", "created_at TEXT NOT NULL", "created_at TEXT NOT NULL"}}},
		{name: "legacy required column name with non-breaking space", rewrites: []articleRewrite{{"articles", ", " + metadataColumn, ""}, {"articles", "created_at TEXT NOT NULL", "created_at TEXT NOT NULL"}}},
		{name: "legacy status literal space", rewrites: []articleRewrite{{"articles", ", " + metadataColumn, ""}, {"articles", statusCheck, "CHECK(status IN ('draft','pub lished'))"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := syntheticCanonical(t, tc.skip, tc.rewrites, tc.extra)
			before, err := sha256File(path)
			if err != nil {
				t.Fatal(err)
			}
			err = articleSchemaMismatch(path, filepath.Join(artifactDir, "events_ru.db"))
			if tc.ok && err != nil {
				t.Errorf("supported article schema rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("unsupported article schema accepted")
			}
			after, err := sha256File(path)
			if err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Error("schema guard modified the canonical file")
			}
		})
	}
}

func TestArticleSchemaGuardEscapedQuotes(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, canonical string
		ok                       bool
	}{
		{name: "escaped quote exact", fixture: `DEFAULT 'it'' s'`, canonical: `DEFAULT 'it'' s'`, ok: true},
		{name: "escaped quote external whitespace", fixture: `DEFAULT 'it'' s'`, canonical: "DEFAULT \n 'it'' s'", ok: true},
		{name: "space after escaped quote", fixture: `DEFAULT 'it'' s'`, canonical: `DEFAULT 'it''s'`},
		{name: "backslash is not an escape", fixture: `DEFAULT 'a\'' b'`, canonical: `DEFAULT 'a\''b'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := syntheticCanonical(t, nil, []articleRewrite{{"articles", "DEFAULT 'draft'", tc.fixture}}, "")
			canonical := syntheticCanonical(t, nil, []articleRewrite{{"articles", "DEFAULT 'draft'", tc.canonical}}, "")
			err := articleSchemaMismatch(canonical, fixture)
			if tc.ok && err != nil {
				t.Errorf("supported article schema rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("unsupported article schema accepted")
			}
		})
	}
}

func TestNormalizeSchemaSQLPreservesQuotedContents(t *testing.T) {
	for _, tc := range []struct{ statement, want string }{
		{"CREATE TABLE notes (\n\tbody TEXT DEFAULT 'a  b'\n)", "CREATE TABLE notes(body TEXT DEFAULT 'a  b')"},
		{`CHECK(note IN ('it'' s', ' x '))`, `CHECK(note IN('it'' s',' x '))`},
		{`DEFAULT 'a\'' b'`, `DEFAULT 'a\'' b'`},
		{`CREATE TABLE "my  notes" ("say "" hi" TEXT)`, `CREATE TABLE "my  notes"("say "" hi" TEXT)`},
		{"CREATE TABLE `my  notes` (`say `` hi` TEXT)", "CREATE TABLE `my  notes`(`say `` hi` TEXT)"},
		{`CREATE TABLE [my  notes] ([say 'hi"] TEXT)`, `CREATE TABLE [my  notes]([say 'hi"] TEXT)`},
		{"body TEXT -- a  note\n  NOT NULL", "body TEXT -- a  note\nNOT NULL"},
	} {
		if got := normalizeSchemaSQL(tc.statement); got != tc.want {
			t.Errorf("normalizeSchemaSQL(%q) = %q, want %q", tc.statement, got, tc.want)
		}
	}
	for _, pair := range [][2]string{
		{"CHECK ( status\n\tIN ( 'draft' , 'published' ) )", "CHECK(status IN('draft','published'))"},
		{"  created_at\tTEXT  NOT NULL  ", "created_at TEXT NOT NULL"},
	} {
		if normalizeSchemaSQL(pair[0]) != normalizeSchemaSQL(pair[1]) {
			t.Errorf("%q and %q must normalize equal", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{
		{`'published'`, `'pub lished'`},
		{`"sort_order"`, `" sort_order"`},
		{"`sort_order`", "`sort_order `"},
		{`[sort_order]`, `[sort  order]`},
		{"created_at TEXT", "created_atTEXT"},
		{"NOT NULL", "NOTNULL"},
		{`'a' 'b'`, `'a''b'`},
		{`"a" "b"`, `"a""b"`},
		{"x -- note\nTEXT", "x -- note TEXT"},
		{"a - -1", "a --1"},
		{"created_at TEXT", "created_at TEXT"},
		{"created_at TEXT", "created_at\u0085TEXT"},
		{"created_at TEXT", "created_at TEXT"},
	} {
		if normalizeSchemaSQL(pair[0]) == normalizeSchemaSQL(pair[1]) {
			t.Errorf("%q and %q must not normalize equal", pair[0], pair[1])
		}
	}
}

func isRUFixture(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM events WHERE id = 4`).Scan(&n)
	return n == 1, err
}

func bootArticleService(t *testing.T, mutate func(*sql.DB) error) string {
	t.Helper()
	base, serviceLog, err := bootService(t, stageArtifact(t, mutate))
	if err != nil {
		t.Fatalf("service did not boot: %v\n--- log ---\n%s", err, serviceLog)
	}
	return base
}

func assertArticleSummary(t *testing.T, where string, summary map[string]json.RawMessage) {
	t.Helper()
	want := set(articleSummaryKeys)
	for key := range summary {
		if !want[key] {
			t.Errorf("%s: summary carries %q", where, key)
		}
	}
	for key := range want {
		if _, ok := summary[key]; !ok {
			t.Errorf("%s: summary lacks %q", where, key)
		}
	}
	var sources []string
	if err := json.Unmarshal(summary["sources"], &sources); err != nil || sources == nil {
		t.Errorf("%s: sources is not a native array: %s", where, summary["sources"])
	}
	if string(summary["published_at"]) == "null" {
		t.Errorf("%s: published summary has no published_at", where)
	}
}

func assertArticleError(t *testing.T, path string, code, wantCode int, body map[string]any, want string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("%s: want %d, got %d %+v", path, wantCode, code, body)
		return
	}
	if body["error"] != want || len(body) != 1 {
		t.Errorf("%s: want only error %q, got %+v", path, want, body)
	}
}

func decodeEventArticles(t *testing.T, path string, raw map[string]json.RawMessage) (*articleDetail, []map[string]json.RawMessage) {
	t.Helper()
	primaryRaw, ok := raw["primary"]
	if !ok {
		t.Fatalf("%s: no primary key: %s", path, raw)
	}
	extraRaw, ok := raw["extra"]
	if !ok || string(extraRaw) == "null" {
		t.Fatalf("%s: extra must be an array: %s", path, raw)
	}
	var primary *articleDetail
	if err := json.Unmarshal(primaryRaw, &primary); err != nil {
		t.Fatalf("%s: primary: %v", path, err)
	}
	var extra []map[string]json.RawMessage
	if err := json.Unmarshal(extraRaw, &extra); err != nil {
		t.Fatalf("%s: extra: %v", path, err)
	}
	return primary, extra
}

func extraKeys(t *testing.T, path string, extra []map[string]json.RawMessage) []string {
	t.Helper()
	keys := []string{}
	for _, summary := range extra {
		assertArticleSummary(t, path, summary)
		var key string
		_ = json.Unmarshal(summary["article_key"], &key)
		keys = append(keys, key)
	}
	return keys
}

func relatedIDs(events []event) []int {
	ids := []int{}
	for _, e := range events {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestPublishedArticleList(t *testing.T) {
	var list struct {
		Articles []map[string]json.RawMessage `json:"articles"`
	}
	if code := getAs(t, apiKey3, "/api/articles?lang=ru", &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if len(list.Articles) != 1 || string(list.Articles[0]["article_key"]) != `"fixture-history"` {
		t.Fatalf("list must contain only published articles: %+v", list)
	}
	assertArticleSummary(t, "list", list.Articles[0])

	var en map[string]json.RawMessage
	if code := getAs(t, apiKey3, "/api/articles?lang=en", &en); code != http.StatusOK || string(en["articles"]) != "[]" {
		t.Fatalf("local language list: %d %s", code, en)
	}
}

func TestPublishedArticleDetail(t *testing.T) {
	var body articleDetailBody
	if code := getAs(t, apiKey3, "/api/articles/history?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	d := body.Data
	if d.ArticleKey != "fixture-history" || d.Slug != "history" || d.Title != "History" || d.Body != "Verified **history**." {
		t.Fatalf("detail: %+v", d)
	}
	if !reflect.DeepEqual(d.Sources, []string{"https://source.example/history"}) || d.PublishedAt == nil {
		t.Errorf("detail summary fields: %+v", d)
	}
	if got := relatedIDs(d.RelatedEvents); !reflect.DeepEqual(got, []int{1, 2, 5, 3}) {
		t.Errorf("related events must sort by date then id: got %v", got)
	}
	if len(d.RelatedEvents) > 0 {
		first := d.RelatedEvents[0]
		if first.URLPath != "/1881-09-29/birthday-of-ludwig-von-mises/" || first.Date != "1881-09-29" || first.Category != "holiday" || !first.Landmark {
			t.Errorf("related events must use the full event shape: %+v", first)
		}
	}
	if d.Translations == nil || len(d.Translations) != 0 {
		t.Errorf("translations must be [] without a published counterpart: %+v", d.Translations)
	}
}

func TestArticleMetadata(t *testing.T) {
	var body articleDetailBody
	if code := getAs(t, apiKey3, "/api/articles/history?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("metadata status: %d", code)
	}
	var metadata struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Image       struct {
			URL    string `json:"url"`
			Alt    string `json:"alt"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Credit string `json:"credit"`
		} `json:"image"`
	}
	if err := json.Unmarshal(body.Data.Metadata, &metadata); err != nil {
		t.Fatalf("metadata is not an object: %s: %v", body.Data.Metadata, err)
	}
	img := metadata.Image
	if metadata.Title != "История" || metadata.Description != "A curated history." ||
		!strings.HasPrefix(img.URL, "https://media.21ideas.org/") || !strings.HasSuffix(img.URL, ".jpg") ||
		img.Alt != "Room77 entrance" || img.Width != 999 || img.Height != 537 || img.Credit != "Photo credit" {
		t.Fatalf("metadata not preserved: %s", body.Data.Metadata)
	}
}

func TestArticleNotFoundAndInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		path string
		code int
		msg  string
	}{
		{"/api/articles/draft?lang=ru", http.StatusNotFound, "Article not found"},
		{"/api/articles/missing?lang=ru", http.StatusNotFound, "Article not found"},
		{"/api/articles/history?lang=en", http.StatusNotFound, "Article not found"},
		{"/api/events/999999/articles?lang=ru", http.StatusNotFound, "Event not found"},
		{"/api/articles/History?lang=ru", http.StatusBadRequest, "Invalid article slug"},
		{"/api/articles/bad_slug?lang=ru", http.StatusBadRequest, "Invalid article slug"},
		{"/api/articles/-history?lang=ru", http.StatusBadRequest, "Invalid article slug"},
		{"/api/articles/history-?lang=ru", http.StatusBadRequest, "Invalid article slug"},
		{"/api/events/abc/articles?lang=ru", http.StatusBadRequest, "Invalid Event ID format"},
		{"/api/events/0/articles?lang=ru", http.StatusBadRequest, "Invalid Event ID format"},
		{"/api/events/-1/articles?lang=ru", http.StatusBadRequest, "Invalid Event ID format"},
	} {
		var body map[string]any
		code := getAs(t, apiKey3, tc.path, &body)
		assertArticleError(t, tc.path, code, tc.code, body, tc.msg)
	}
}

func TestEventArticlesPrimaryAndExtra(t *testing.T) {
	fetch := func(path string) (*articleDetail, []map[string]json.RawMessage, string) {
		t.Helper()
		var raw map[string]json.RawMessage
		if code := getAs(t, apiKey3, path, &raw); code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		primary, extra := decodeEventArticles(t, path, raw)
		encoded, _ := json.Marshal(raw)
		return primary, extra, string(encoded)
	}

	primary, extra, _ := fetch("/api/events/2/articles?lang=ru")
	if primary == nil || primary.ArticleKey != "fixture-history" || primary.Body != "Verified **history**." || len(extra) != 0 {
		t.Fatalf("event 2: %+v %+v", primary, extra)
	}
	if got := relatedIDs(primary.RelatedEvents); !reflect.DeepEqual(got, []int{1, 2, 5, 3}) {
		t.Errorf("embedded primary related events: got %v", got)
	}
	if primary.Translations == nil || len(primary.Metadata) == 0 || string(primary.Metadata) == "null" {
		t.Errorf("embedded primary must be the full detail: %+v", primary)
	}

	primary, extra, encoded := fetch("/api/events/3/articles?lang=ru")
	if primary != nil {
		t.Errorf("draft primary leaked: %+v", primary)
	}
	if got := extraKeys(t, "event 3 extra", extra); !reflect.DeepEqual(got, []string{"fixture-history"}) {
		t.Errorf("event 3 extras: %v", got)
	}
	for _, secret := range []string{"fixture-draft", "Secret draft", "Never expose me"} {
		if strings.Contains(encoded, secret) {
			t.Errorf("event 3 leaks draft %q: %s", secret, encoded)
		}
	}

	primary, extra, _ = fetch("/api/events/5/articles?lang=ru")
	if primary != nil {
		t.Errorf("event 5 has no primary relation: %+v", primary)
	}
	if got := extraKeys(t, "event 5 extra", extra); !reflect.DeepEqual(got, []string{"fixture-history"}) {
		t.Errorf("event 5 extras: %v", got)
	}

	for _, path := range []string{"/api/events/4/articles?lang=ru", "/api/events/2/articles?lang=en"} {
		primary, extra, _ = fetch(path)
		if primary != nil || len(extra) != 0 {
			t.Errorf("%s: want null primary and no extras, got %+v %+v", path, primary, extra)
		}
	}
}

func TestEventsTagNamedArticlesIsATag(t *testing.T) {
	path := "/api/events/tags/articles?lang=ru"
	var empty struct {
		Events     json.RawMessage `json:"events"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	if code := getAs(t, apiKey3, path, &empty); code != http.StatusOK {
		t.Fatalf("%s: tag lookup must not parse the tag as an event id, got %d", path, code)
	}
	if string(empty.Events) != "[]" || empty.Pagination.Total != 0 {
		t.Errorf("%s: want an empty tag list, got %s total %d", path, empty.Events, empty.Pagination.Total)
	}

	base := bootArticleService(t, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE events SET tags='["satoshi", "articles"]' WHERE id=2`)
		return err
	})
	var tagged eventList
	if code := getFrom(t, base, path, &tagged); code != http.StatusOK {
		t.Fatalf("%s on a tagged fixture: %d", path, code)
	}
	if got := relatedIDs(tagged.Events); !reflect.DeepEqual(got, []int{2}) || tagged.Pagination.Total != 1 {
		t.Errorf("%s: want event 2 only, got %v total %d", path, got, tagged.Pagination.Total)
	}

	var raw map[string]json.RawMessage
	relationPath := "/api/events/2/articles?lang=ru"
	if code := getFrom(t, base, relationPath, &raw); code != http.StatusOK {
		t.Fatalf("%s beside the tag route: %d", relationPath, code)
	}
	if primary, _ := decodeEventArticles(t, relationPath, raw); primary == nil || primary.ArticleKey != "fixture-history" {
		t.Errorf("%s: primary: %+v", relationPath, primary)
	}
	var body map[string]any
	code := getFrom(t, base, "/api/events/abc/articles?lang=ru", &body)
	assertArticleError(t, "invalid event id beside the tag route", code, http.StatusBadRequest, body, "Invalid Event ID format")
}

func TestArticleCapabilityAndLegacyEvents(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		_, err := db.Exec("DROP TABLE event_articles; DROP TABLE articles")
		return err
	})
	for _, path := range []string{"/api/articles?lang=ru", "/api/articles/history?lang=ru", "/api/events/2/articles?lang=ru"} {
		var body map[string]any
		code := getFrom(t, base, path, &body)
		assertArticleError(t, path, code, http.StatusServiceUnavailable, body, "Articles unavailable for this artifact")
	}
	var body map[string]any
	code := getFrom(t, base, "/api/events/999999/articles?lang=ru", &body)
	assertArticleError(t, "missing event on old artifact", code, http.StatusNotFound, body, "Event not found")

	for _, path := range []string{"/health", "/api/events/2?lang=ru", "/api/events?lang=ru", "/api/search?q=bitcoin&lang=ru", "/public/v1/events?lang=ru", "/public/v2/events?lang=ru"} {
		if code := getFrom(t, base, path, nil); code != http.StatusOK {
			t.Errorf("%s on an artifact without articles: %d", path, code)
		}
	}
}

func TestPartialArticleSchemaIsUnavailable(t *testing.T) {
	for _, tc := range []struct{ name, ddl string }{
		{"relations table missing", "DROP TABLE event_articles"},
		{"articles column missing", "ALTER TABLE articles DROP COLUMN sources"},
		{"relations column missing", "ALTER TABLE event_articles DROP COLUMN sort_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := bootArticleService(t, func(db *sql.DB) error {
				_, err := db.Exec(tc.ddl)
				return err
			})
			for _, path := range []string{"/api/articles?lang=ru", "/api/articles/history?lang=ru", "/api/events/2/articles?lang=ru"} {
				var body map[string]any
				code := getFrom(t, base, path, &body)
				assertArticleError(t, path, code, http.StatusServiceUnavailable, body, "Articles unavailable for this artifact")
			}
			if code := getFrom(t, base, "/api/events/2?lang=ru", nil); code != http.StatusOK {
				t.Errorf("event route on a partial article schema: %d", code)
			}
		})
	}
}

func TestArticleQueryFailureIsServerError(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		_, err := db.Exec(`ALTER TABLE articles RENAME TO articles_stored;
CREATE VIEW articles AS SELECT article_key, slug, json_extract(title, '$') AS title, body_markdown, sources, status, created_at, updated_at, published_at, metadata FROM articles_stored;`)
		return err
	})
	for _, path := range []string{"/api/articles?lang=ru", "/api/articles/history?lang=ru", "/api/events/2/articles?lang=ru"} {
		var body map[string]any
		code := getFrom(t, base, path, &body)
		if code != http.StatusInternalServerError {
			t.Errorf("%s: query failure must be 500, got %d %+v", path, code, body)
			continue
		}
		msg, _ := body["error"].(string)
		if msg == "" {
			t.Errorf("%s: 500 has no public error message", path)
		}
		for _, detail := range []string{"malformed", "sqlite", "json_extract", "articles_stored"} {
			if strings.Contains(strings.ToLower(msg), detail) {
				t.Errorf("%s: public error leaks database detail %q: %q", path, detail, msg)
			}
		}
	}
}

func TestArticleTranslationsUseStableKeyNotSlug(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		ru, err := isRUFixture(db)
		if err != nil || ru {
			return err
		}
		_, err = db.Exec(`INSERT INTO articles(article_key,slug,title,body_markdown,sources,status,created_at,updated_at,published_at) VALUES
 ('other-key','history','Wrong article','Different identity','[]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z'),
 ('fixture-history','draft-history','Draft translation','Unpublished translation','[]','draft','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z',NULL)`)
		return err
	})
	var ru articleDetailBody
	if code := getFrom(t, base, "/api/articles/history?lang=ru", &ru); code != http.StatusOK || ru.Data.ArticleKey != "fixture-history" {
		t.Fatalf("ru detail: %d %+v", code, ru)
	}
	if ru.Data.Translations == nil || len(ru.Data.Translations) != 0 {
		t.Errorf("translation guessed by slug or from a draft: %+v", ru.Data.Translations)
	}
	var en articleDetailBody
	if code := getFrom(t, base, "/api/articles/history?lang=en", &en); code != http.StatusOK || en.Data.ArticleKey != "other-key" {
		t.Fatalf("en detail must read the local artifact: %d %+v", code, en)
	}
	if en.Data.Translations == nil || len(en.Data.Translations) != 0 || en.Data.RelatedEvents == nil || len(en.Data.RelatedEvents) != 0 {
		t.Errorf("en detail must not join by slug or numeric id: %+v", en.Data)
	}
	var body map[string]any
	code := getFrom(t, base, "/api/articles/draft-history?lang=en", &body)
	assertArticleError(t, "draft translation", code, http.StatusNotFound, body, "Article not found")
}

func TestArticleTranslationsWithoutCounterpartSchema(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		ru, err := isRUFixture(db)
		if err != nil || ru {
			return err
		}
		_, err = db.Exec("DROP TABLE event_articles; DROP TABLE articles")
		return err
	})
	var body articleDetailBody
	if code := getFrom(t, base, "/api/articles/history?lang=ru", &body); code != http.StatusOK {
		t.Fatalf("ru detail beside an old en artifact: %d", code)
	}
	if body.Data.Translations == nil || len(body.Data.Translations) != 0 {
		t.Errorf("translations: %+v", body.Data.Translations)
	}
}

func TestVerifiedArticleTranslationAndExtraOrdering(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		ru, err := isRUFixture(db)
		if err != nil {
			return err
		}
		if !ru {
			_, err := db.Exec(`INSERT INTO articles(article_key,slug,title,body_markdown,sources,status,created_at,updated_at,published_at) VALUES('fixture-history','translated-history','Translated title','Translated body','[]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z')`)
			return err
		}
		if _, err := db.Exec(`INSERT INTO articles(article_key,slug,title,body_markdown,sources,status,created_at,updated_at,published_at) VALUES
 ('key-zeta','zeta','Zeta','Zeta body must not appear in a summary','[]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z'),
 ('key-alpha','alpha','Alpha','Alpha body must not appear in a summary','["https://source.example/alpha"]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z'),
 ('key-beta','beta','Beta','Beta body must not appear in a summary','[]','published','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z','2026-10-18T00:00:00Z');
 INSERT INTO event_articles(event_id,article_key,is_primary,sort_order) VALUES(2,'key-beta',0,1),(2,'fixture-draft',0,0),(2,'key-alpha',0,1),(2,'key-zeta',0,0)`); err != nil {
			return err
		}
		_, err = db.Exec(`UPDATE articles SET metadata=? WHERE article_key='key-alpha'`, articleMetadata)
		return err
	})

	var raw map[string]json.RawMessage
	path := "/api/events/2/articles?lang=ru"
	if code := getFrom(t, base, path, &raw); code != http.StatusOK {
		t.Fatalf("relation status: %d", code)
	}
	primary, extra := decodeEventArticles(t, path, raw)
	if primary == nil || primary.ArticleKey != "fixture-history" {
		t.Fatalf("primary: %+v", primary)
	}
	if !reflect.DeepEqual(primary.Translations, []articleTranslation{{Locale: "en", Slug: "translated-history"}}) {
		t.Errorf("translation: %+v", primary.Translations)
	}
	if got := extraKeys(t, "extra", extra); !reflect.DeepEqual(got, []string{"key-zeta", "key-alpha", "key-beta"}) {
		t.Errorf("extras must sort by sort_order then slug and omit drafts: %v", got)
	}
	encoded, _ := json.Marshal(raw["extra"])
	for _, leaked := range []string{"must not appear", "A curated history.", "fixture-draft"} {
		if strings.Contains(string(encoded), leaked) {
			t.Errorf("extra summaries leak %q: %s", leaked, encoded)
		}
	}

	var en articleDetailBody
	if code := getFrom(t, base, "/api/articles/translated-history?lang=en", &en); code != http.StatusOK {
		t.Fatalf("en detail: %d", code)
	}
	if !reflect.DeepEqual(en.Data.Translations, []articleTranslation{{Locale: "ru", Slug: "history"}}) {
		t.Errorf("reverse translation: %+v", en.Data.Translations)
	}
	if en.Data.RelatedEvents == nil || len(en.Data.RelatedEvents) != 0 {
		t.Errorf("en related events must be local: %+v", en.Data.RelatedEvents)
	}

	var zeta articleDetailBody
	if code := getFrom(t, base, "/api/articles/zeta?lang=ru", &zeta); code != http.StatusOK || zeta.Data.Body == "" {
		t.Fatalf("zeta detail: %d %+v", code, zeta)
	}
	if len(zeta.Data.Metadata) != 0 && string(zeta.Data.Metadata) != "null" {
		t.Errorf("article without metadata must not invent it: %s", zeta.Data.Metadata)
	}
}

func TestLegacyArticleWithoutMetadata(t *testing.T) {
	base := bootArticleService(t, func(db *sql.DB) error {
		_, err := db.Exec("ALTER TABLE articles DROP COLUMN metadata")
		return err
	})
	var body articleDetailBody
	if code := getFrom(t, base, "/api/articles/history?lang=ru", &body); code != http.StatusOK || body.Data.Body != "Verified **history**." {
		t.Fatalf("legacy article: %d %+v", code, body)
	}
	if len(body.Data.Metadata) != 0 && string(body.Data.Metadata) != "null" {
		t.Errorf("legacy article invents metadata: %s", body.Data.Metadata)
	}
	var list struct {
		Articles []map[string]json.RawMessage `json:"articles"`
	}
	if code := getFrom(t, base, "/api/articles?lang=ru", &list); code != http.StatusOK || len(list.Articles) != 1 {
		t.Fatalf("legacy list: %d %+v", code, list)
	}
	assertArticleSummary(t, "legacy list", list.Articles[0])
	var raw map[string]json.RawMessage
	path := "/api/events/5/articles?lang=ru"
	if code := getFrom(t, base, path, &raw); code != http.StatusOK {
		t.Fatalf("legacy relation: %d", code)
	}
	_, extra := decodeEventArticles(t, path, raw)
	if got := extraKeys(t, "legacy extra", extra); !reflect.DeepEqual(got, []string{"fixture-history"}) {
		t.Errorf("legacy extras: %v", got)
	}
}
