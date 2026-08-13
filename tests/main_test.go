package tests

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	apiKey = "test-key"
	// A second valid key, so the suite can prove the rate limiter gives each
	// caller its own budget rather than one shared per-IP bucket.
	apiKey2 = "test-key-2"
	// A third, for the tests that sweep a matrix of parameters. The limiter
	// allows 100 requests a minute per key, and a table of a dozen probes across
	// three endpoints spends enough of the default key's budget to leave later
	// tests answering 429 — a failure that reads like a bug in whatever ran
	// last. Per-key budgets are the point of the limiter's design, so the fix is
	// to use one rather than to trim the coverage.
	apiKey3       = "test-key-3"
	allowedOrigin = "http://localhost:3000"
)

var (
	baseURL     string
	artifactDir string
	fixtureSums map[string]string // filename -> sha256 at publish time
	binaryPath  string            // built once by run(); reused to boot extra instances
)

// TestMain stages an artifact the way a release does, starts the real binary
// against it, and leaves it running for the whole suite.
func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "test setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	workDir, err := os.MkdirTemp("", "bitcal-tests-")
	if err != nil {
		return 0, err
	}
	// The artifact directory is made unwritable below; restore it so the
	// cleanup can unlink the files inside.
	defer func() {
		_ = os.Chmod(artifactDir, 0o755)
		_ = os.RemoveAll(workDir)
	}()

	binary := filepath.Join(workDir, "bitcal-api")
	binaryPath = binary
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binary, ".")
	build.Dir = ".."
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("building the binary failed — this is what a missing -tags fts5 looks like:\n%s", out)
	}

	artifactDir = filepath.Join(workDir, "artifact")
	if err := os.Mkdir(artifactDir, 0o755); err != nil {
		return 0, err
	}
	for _, lang := range []string{"en", "ru"} {
		if err := seedArtifact(filepath.Join(artifactDir, "events_"+lang+".db"), lang); err != nil {
			return 0, fmt.Errorf("seeding %s: %w", lang, err)
		}
	}

	// The deployment condition, and not decoration: files read-only to
	// everyone, in a directory the service user cannot write either.
	matches, _ := filepath.Glob(filepath.Join(artifactDir, "*.db"))
	fixtureSums = map[string]string{}
	for _, f := range matches {
		if err := os.Chmod(f, 0o444); err != nil {
			return 0, err
		}
		sum, err := sha256File(f)
		if err != nil {
			return 0, err
		}
		fixtureSums[filepath.Base(f)] = sum
	}
	if err := os.Chmod(artifactDir, 0o555); err != nil {
		return 0, err
	}

	port, err := freePort()
	if err != nil {
		return 0, err
	}
	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	srv := exec.Command(binary)
	srv.Env = append(os.Environ(),
		fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", port),
		"DB_PATH_EN="+filepath.Join(artifactDir, "events_en.db"),
		"DB_PATH_RU="+filepath.Join(artifactDir, "events_ru.db"),
		"API_KEYS="+apiKey+","+apiKey2+","+apiKey3,
		"CORS_ALLOWED_ORIGINS="+allowedOrigin,
	)
	log, err := os.Create(filepath.Join(workDir, "server.log"))
	if err != nil {
		return 0, err
	}
	srv.Stdout, srv.Stderr = log, log
	if err := srv.Start(); err != nil {
		return 0, err
	}
	// If the service exits — which is what a read-only violation looks like —
	// say so at once instead of polling a dead port for half a minute and then
	// reporting the wrong thing. The channel is closed rather than sent on, so
	// that both the startup check and the cleanup below can observe it; a
	// single send would let whichever ran first starve the other.
	var exitErr error
	exited := make(chan struct{})
	go func() { exitErr = srv.Wait(); close(exited) }()
	defer func() {
		_ = srv.Process.Kill()
		<-exited
	}()

	if err := waitForHealth(baseURL, exited, &exitErr, 30*time.Second); err != nil {
		out, _ := os.ReadFile(filepath.Join(workDir, "server.log"))
		return 0, fmt.Errorf("%w\n--- server log ---\n%s", err, out)
	}

	return m.Run(), nil
}

// seedArtifact writes a database with the canonical schema — including the
// declared column types, which is what makes the driver hand back a time.Time
// for date, and the FTS5 index and triggers the service must never create.
func seedArtifact(path, lang string) error {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return err
	}
	defer db.Close()

	// `landmark INTEGER NOT NULL DEFAULT 0` is exactly how canonical declares
	// it, because the declared type is what the driver hands back and a nullable
	// column would let a NULL reach a Go bool — proving nothing about the shape
	// the service actually meets.
	//
	// That annotation is here rather than as a `--` comment inside the statement
	// below, and that is not style. SQLite stores the CREATE TABLE text verbatim
	// in sqlite_master and re-parses it during ALTER TABLE ... DROP COLUMN,
	// which truncates at the comment: the rollback tests that drop `category`
	// and `landmark` then fail with `error in table events after drop column:
	// incomplete input`. Measured — it is how this comment came to be out here.
	schema := []string{
		`CREATE TABLE events (
			id INTEGER PRIMARY KEY,
			date date NOT NULL,
			title TEXT,
			description TEXT,
			media TEXT,
			tags TEXT,
			"references" TEXT,
			created_at datetime,
			updated_at datetime,
			url_path TEXT,
			category TEXT,
			landmark INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_events_date ON events(date)`,
		`CREATE UNIQUE INDEX idx_events_url_path ON events(url_path)`,
		`CREATE VIRTUAL TABLE events_fts USING fts5(
			title, description, tags, content='events', content_rowid='id')`,
		`CREATE TRIGGER events_ai AFTER INSERT ON events BEGIN
			INSERT INTO events_fts(rowid, title, description, tags)
			VALUES (new.id, new.title, new.description, new.tags);
		END`,
		`CREATE TRIGGER events_ad AFTER DELETE ON events BEGIN
			INSERT INTO events_fts(events_fts, rowid, title, description, tags)
			VALUES('delete', old.id, old.title, old.description, old.tags);
		END`,
		`CREATE TRIGGER events_au AFTER UPDATE ON events BEGIN
			INSERT INTO events_fts(events_fts, rowid, title, description, tags)
			VALUES('delete', old.id, old.title, old.description, old.tags);
			INSERT INTO events_fts(rowid, title, description, tags)
			VALUES (new.id, new.title, new.description, new.tags);
		END`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s[:40], err)
		}
	}

	for _, e := range fixtureRows(lang) {
		if _, err := db.Exec(
			`INSERT INTO events (id, date, title, description, media, tags, "references", created_at, updated_at, url_path, category, landmark)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.Date, e.Title, e.Description, e.Media, e.Tags, e.References, e.CreatedAt, e.UpdatedAt, e.URLPath, e.Category, e.Landmark,
		); err != nil {
			return err
		}
	}

	// Leave nothing beside the .db: a WAL header or a journal makes the file
	// unreadable at 0444, which is the invariant a release must hold.
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		return err
	}
	return nil
}

type fixtureRow struct {
	ID                   int
	Date                 string
	Title, Description   string
	Media, References    *string
	CreatedAt, UpdatedAt *string
	Tags, URLPath        string
	// Mandatory in canonical by validator invariant 13 — one value per row from
	// a closed set — so every fixture row carries one. The set is owned by the
	// data and changes in both directions (it gained `security` on 2026-08-10
	// and was rewritten from fifteen values to eight on 2026-08-12), so nothing
	// here asserts its size; TestFixtureSchemaMatchesCanonical compares columns,
	// not values, for exactly that reason.
	//
	// These values are deliberately NOT resynchronised with canonical's current
	// eight. `mustread` and `bitcoin` below were real members until 2026-08-12
	// and are not any more, which changes nothing these tests prove: the fixture
	// is self-contained, the service holds no list to disagree with it, and the
	// schema guard compares columns rather than values. Retargeting them would
	// churn every assertion in category_test.go to prove the same properties.
	// See syntheticCategory, whose argument this does slightly change.
	Category string
	// Landmark is canonical's `landmark`, added 2026-08-12: a boolean, orthogonal
	// to Category, and NOT NULL DEFAULT 0 upstream so it has exactly one spelling
	// for false.
	//
	// Event 2 must keep this true. It is the only row `q=bitcoin` matches in the
	// RU fixture, so it is the only row through which /api/search can prove it
	// fetched the column at all — see TestEventStructCoversEveryColumn, which a
	// false cannot satisfy because false is also what an unfetched column
	// marshals to.
	Landmark bool
}

// syntheticCategory is deliberately NOT a member of canonical's vocabulary, and
// it is the only assertion in this suite that can tell the boot-derived
// vocabulary apart from a hardcoded list.
//
// When this row was written, every other fixture category — holiday, mustread,
// archives, bitcoin — was a real canonical value, so a compiled-in list would
// have contained all of them and accepted all of them. Replacing loadCategories
// with such a list was measured against this suite before this row existed: `go
// vet` clean, every test green. The design the filter exists for was pinned by
// nothing.
//
// Canonical's 2026-08-12 rewrite left only holiday and archives from that set —
// mustread and bitcoin are no longer members either. That weakens the sentence
// above without weakening the row: this value was never a member and never will
// be, whichever direction the vocabulary moves next, so it remains the one
// assertion here that a hardcoded list cannot satisfy by accident.
//
// So one row carries a value no hardcoded list could ever hold. The filter
// accepts it only if the vocabulary really was read out of the artifact at boot,
// which is what TestVocabularyComesFromTheArtifactNotTheBinary asserts.
//
// This is the same trade the duplicated-tag row (event 3) makes: a fixture
// deliberately harsher than production, because a test that cannot fail reads
// like coverage while providing none. It is safe here for the same reason the
// filter is written the way it is — nothing in the service carries a list of
// permitted values to disagree with.
const syntheticCategory = "not-a-canonical-category"

func strptr(s string) *string { return &s }

// fixtureRows deliberately includes the awkward cases: a pre-epoch date, absent
// media and references as NULL, absent timestamps, and an event that lists the
// same tag twice.
//
// Landmark is true on events 1 and 2 and false on the rest — RU 2 of 5, EN 2 of
// 4. Both states have to exist for ?landmark= to be provably a filter rather
// than a pass-through, and the two languages differ so that a handler reading
// the wrong artifact cannot go unnoticed.
func fixtureRows(lang string) []fixtureRow {
	rows := []fixtureRow{
		{
			ID: 1, Date: "1881-09-29",
			Title:       "Birthday of Ludwig von Mises",
			Description: "Born before the Unix epoch, which is the point of this row.",
			Media:       nil, References: nil,
			CreatedAt: nil, UpdatedAt: nil,
			// `price` is exactly five characters on purpose: it is what makes
			// the LIKE-wildcard probe in TestTagFilterIgnoresLikeWildcards bite.
			// Without a five-letter tag here, `_____` matches nothing whether or
			// not the bug is present, and the test passes vacuously.
			Tags: `["holiday", "prebitcoin", "price"]`, URLPath: "/1881-09-29/birthday-of-ludwig-von-mises/",
			// Not `price`, though the row carries that tag: category is a single
			// mandatory classification and tag order no longer implies it. A
			// fixture whose category always matched tags[0] would let the very
			// inference canonical retired keep passing its tests.
			Category: "holiday",
			// One of two landmarks in this fixture. Two rather than one so that
			// ?landmark=true returning the right set cannot be satisfied by a
			// handler that happens to return a single row for another reason.
			Landmark: true,
		},
		{
			ID: 2, Date: "2008-11-01",
			Title:       "Bitcoin whitepaper published",
			Description: "Satoshi Nakamoto publishes the bitcoin whitepaper.",
			Media:       strptr(`["https://example.org/whitepaper.png"]`),
			References:  strptr(`["https://bitcoin.org/bitcoin.pdf"]`),
			CreatedAt:   strptr("2026-08-08 09:59:56"), UpdatedAt: strptr("2026-08-08 09:59:56"),
			Tags: `["satoshi", "mustread"]`, URLPath: "/2008-11-01/bitcoin-whitepaper-published/",
			Category: "mustread",
			// Load-bearing, and not interchangeable with the other landmark: this
			// is the only row `q=bitcoin` matches in either fixture, so it is the
			// only one through which TestEventStructCoversEveryColumn can prove
			// /api/search fetched the landmark column. Set this to false and that
			// test goes green against a search handler that never selects it.
			Landmark: true,
		},
		{
			ID: 3, Date: "2013-08-09",
			Title: "A duplicated tag lives here",
			// Canonical had four such rows when this fixture was written. It has
			// none today — the 2026-08-10 tag cleanup deduplicated them — so this
			// row no longer mirrors the data, and that is deliberate.
			//
			// It stays because the defect it guards is in the handler, not in the
			// data: /api/tags counted occurrences while /api/events/tags/:tag
			// counted events, so the two disagreed by one for any tag listed
			// twice. Removing the duplicate would leave
			// TestTagCountsEventsNotOccurrences unable to fail, which is worse
			// than a fixture that is deliberately harsher than production — a
			// test that cannot fail reads like coverage while providing none.
			// Nothing stops canonical reintroducing a duplicate tomorrow.
			Description: "This row lists satoshi twice; canonical no longer does, but the handler must still count events.",
			Media:       nil, References: nil,
			CreatedAt: nil, UpdatedAt: nil,
			Tags: `["satoshi", "archives", "satoshi"]`, URLPath: "/2013-08-09/a-duplicated-tag-lives-here/",
			Category: "archives",
		},
		{
			// Shares 2008-11-01 with event 2, and exists only for that. The
			// English artifact has 19 dates carrying more than one event, so a
			// sort on the date alone leaves real ties unbroken — and SQL promises
			// nothing about the order of rows it cannot separate. Without a tied
			// pair here, TestListOrderBreaksTiesById cannot fail, and a test that
			// cannot fail reads like coverage while providing none.
			//
			// It deliberately carries neither `satoshi` in its tags nor
			// `whitepaper`/`published` in its text: the tag counts and the phrase
			// search assertions are pinned to exact numbers elsewhere.
			ID: 5, Date: "2008-11-01",
			Title:       "A second event on the same day",
			Description: "Two events share this date, so the sort has a tie to break.",
			Media:       nil, References: nil,
			CreatedAt: nil, UpdatedAt: nil,
			Tags: `["archives"]`, URLPath: "/2008-11-01/a-second-event-on-the-same-day/",
			// Same tag as event 3 but a different category, so that nothing can
			// pass by treating the two fields as interchangeable — and the one
			// value in this fixture that canonical does not carry, so that the
			// vocabulary's origin is provable. See syntheticCategory.
			Category: syntheticCategory,
		},
	}
	if lang == "ru" {
		rows = append(rows, fixtureRow{
			ID: 4, Date: "2020-12-08",
			Title:       "Только в русской базе",
			Description: "Идентификаторы независимы в каждом языке.",
			Media:       nil, References: nil,
			CreatedAt: nil, UpdatedAt: nil,
			Tags: `["satoshi"]`, URLPath: "/2020-12-08/ru-only/",
			// The tag and the category deliberately differ: `bitcoin` is a
			// category with no corresponding tag anywhere in canonical, which is
			// precisely the pairing that would break a consumer still deriving
			// category from tags[0].
			Category: "bitcoin",
		})
	}
	return rows
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForHealth blocks until the service answers /health, and distinguishes the
// three ways that can fail. Each has a different cause and a different fix, so
// collapsing them into one timeout message wastes the reader's time.
func waitForHealth(base string, exited <-chan struct{}, exitErr *error, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error

	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("the service exited during startup (%v) — read the "+
				"log below; a read-only violation looks exactly like this", *exitErr)
		default:
		}

		res, err := http.Get(base + "/health")
		if err == nil {
			res.Body.Close()
			switch res.StatusCode {
			case http.StatusOK:
				return nil
			case http.StatusUnauthorized, http.StatusForbidden:
				return fmt.Errorf("/health answered %d: it must be registered "+
					"outside the /api group and require no API key — a deploy "+
					"check that needs a secret is a deploy check that gets skipped",
					res.StatusCode)
			default:
				last = fmt.Errorf("/health returned %d", res.StatusCode)
			}
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the service never answered /health within %s: %w", within, last)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get issues an authenticated request and decodes the JSON body.
func get(t *testing.T, path string, into interface{}) int {
	t.Helper()
	return request(t, http.MethodGet, path, true, into)
}

// getAs is get under a nominated key, for tests that would otherwise spend the
// default key's rate-limit budget on behalf of every test that follows them.
func getAs(t *testing.T, key, path string, into interface{}) int {
	t.Helper()
	return requestAs(t, http.MethodGet, path, key, into)
}

func request(t *testing.T, method, path string, auth bool, into interface{}) int {
	t.Helper()
	key := ""
	if auth {
		key = apiKey
	}
	return requestAs(t, method, path, key, into)
}

// requestAs sends the request under the given key, or unauthenticated when the
// key is empty.
func requestAs(t *testing.T, method, path, key string, into interface{}) int {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if key != "" {
		req.Header.Set("X-API-KEY", key)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	switch {
	case into == nil:
		_, _ = io.Copy(io.Discard, res.Body)

	case res.StatusCode == http.StatusOK:
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("%s %s: decoding body: %v", method, path, err)
		}

	case strings.Contains(res.Header.Get("Content-Type"), "application/json"):
		// Error responses are JSON too, and the tests that assert a 400 also
		// assert the message explains itself. A decode failure here is not
		// fatal: the status code is the primary assertion, the body advisory.
		_ = json.NewDecoder(res.Body).Decode(into)

	default:
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return res.StatusCode
}

// event mirrors the JSON the service emits. Pointers where the contract says a
// field may be null, so that "absent" and "empty" stay distinguishable.
type event struct {
	ID          int     `json:"id"`
	Date        string  `json:"date"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Tags        string  `json:"tags"`
	Media       *string `json:"media"`
	References  *string `json:"references"`
	URLPath     string  `json:"url_path"`
	Category    string  `json:"category"`
	// Not a pointer, deliberately, mirroring the service: the contract says
	// landmark is always a bool, never null. Decoding into a plain bool is also
	// what would catch the service starting to emit null — encoding/json would
	// leave it false and the assertions that expect true would fail, which is
	// the direction that gets noticed.
	Landmark  bool    `json:"landmark"`
	CreatedAt *string `json:"created_at"`
	UpdatedAt *string `json:"updated_at"`
}

type eventList struct {
	Events     []event `json:"events"`
	Pagination struct {
		CurrentPage int `json:"current_page"`
		PerPage     int `json:"per_page"`
		Total       int `json:"total"`
		LastPage    int `json:"last_page"`
	} `json:"pagination"`
}

type tagInfo struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}
