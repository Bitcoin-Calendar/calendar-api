package tests

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// healthDoc mirrors /health. It is spelled out here rather than imported from
// the service because that is the point of a black-box test: if the JSON the
// publisher reads changes shape, this fails to unmarshal.
type healthDoc struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Databases map[string]struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Rows   int64  `json:"rows"`
		FTS    struct {
			Indexed    int64 `json:"indexed"`
			Consistent bool  `json:"consistent"`
		} `json:"fts"`
		Categories struct {
			Present bool `json:"present"`
			Count   int  `json:"count"`
		} `json:"categories"`
		Landmark struct {
			Present bool  `json:"present"`
			Count   int64 `json:"count"`
		} `json:"landmark"`
	} `json:"databases"`
}

// stageArtifact builds a fresh pair of databases in their own directory, runs
// the given mutation against each, and stages them exactly as a release does:
// mode 0444 in a 0555 directory. A nil mutation leaves a healthy artifact.
func stageArtifact(t *testing.T, mutate func(*sql.DB) error) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "artifact")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating artifact dir: %v", err)
	}
	// Registered after TempDir's own cleanup and therefore run before it:
	// the directory must be writable again for the temp dir to be removed.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	for _, lang := range []string{"en", "ru"} {
		path := filepath.Join(dir, "events_"+lang+".db")
		if err := seedArtifact(path, lang); err != nil {
			t.Fatalf("seeding %s: %v", lang, err)
		}
		if mutate != nil {
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatalf("opening %s: %v", lang, err)
			}
			err = mutate(db)
			db.Close()
			if err != nil {
				t.Fatalf("mutating %s: %v", lang, err)
			}
		}
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatalf("chmod %s: %v", lang, err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	return dir
}

// instance is a running copy of the real binary, started by the tests.
type instance struct {
	base    string
	proc    *os.Process
	logPath string
	exited  <-chan struct{}
	exitErr *error // valid only once exited is closed
}

// log reads whatever the service has written so far.
func (in *instance) log() string {
	out, _ := os.ReadFile(in.logPath)
	return string(out)
}

// signal delivers sig and waits for the process to go away, returning the
// error from Wait — nil means it exited 0 of its own accord.
func (in *instance) signal(t *testing.T, sig os.Signal, within time.Duration) error {
	t.Helper()
	if err := in.proc.Signal(sig); err != nil {
		t.Fatalf("sending %v: %v", sig, err)
	}
	select {
	case <-in.exited:
		return *in.exitErr
	case <-time.After(within):
		t.Fatalf("the service ignored %v for %s", sig, within)
		return nil
	}
}

// startService starts a second instance of the real binary against the given
// artifact directory and waits for it to become healthy. The instance is
// returned even when startErr is non-nil, so the caller can read its log. The
// process is killed on cleanup whichever happened.
func startService(t *testing.T, dir string) (*instance, error) {
	t.Helper()

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}

	logPath := filepath.Join(t.TempDir(), "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating log: %v", err)
	}
	defer logFile.Close()

	srv := exec.Command(binaryPath)
	srv.Env = append(os.Environ(),
		fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", port),
		"DB_PATH_EN="+filepath.Join(dir, "events_en.db"),
		"DB_PATH_RU="+filepath.Join(dir, "events_ru.db"),
		"API_KEYS="+apiKey,
	)
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		t.Fatalf("starting the service: %v", err)
	}

	var exitErr error
	exited := make(chan struct{})
	go func() { exitErr = srv.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		<-exited
	})

	in := &instance{
		base:    fmt.Sprintf("http://127.0.0.1:%d", port),
		proc:    srv.Process,
		logPath: logPath,
		exited:  exited,
		exitErr: &exitErr,
	}

	// Short: every outcome under test is decided within a second or two of
	// opening the databases, and a wrong answer here should not cost 30s.
	return in, waitForHealth(in.base, exited, &exitErr, 15*time.Second)
}

// bootService is the shape most of these tests want: base URL, log, and why
// startup failed if it did.
func bootService(t *testing.T, dir string) (base, serviceLog string, startErr error) {
	t.Helper()
	in, err := startService(t, dir)
	return in.base, in.log(), err
}

// TestBootProbeRejectsUnusableFTS is the reason the probe exists. Each mutation
// here leaves a database that opens perfectly and answers every endpoint except
// search — and search answers too, with an empty result that is indistinguishable
// from "no matches". Nothing downstream can tell the difference: the Telegram
// bot would simply post nothing and report no error. Refusing to boot is the
// only moment at which any of this is loud.
func TestBootProbeRejectsUnusableFTS(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*sql.DB) error
		expect string // must appear in the service's log
	}{
		{
			// An artifact rebuilt by a tool without the FTS5 extension, or one
			// where the index was dropped to save space.
			name: "index missing",
			mutate: func(db *sql.DB) error {
				for _, s := range []string{
					`DROP TRIGGER events_ai`, `DROP TRIGGER events_ad`,
					`DROP TRIGGER events_au`, `DROP TABLE events_fts`,
				} {
					if _, err := db.Exec(s); err != nil {
						return fmt.Errorf("%s: %w", s, err)
					}
				}
				return nil
			},
			expect: "events_fts is missing",
		},
		{
			// The table is present and queryable; it just holds nothing. This
			// is the case a "does events_fts exist?" check would pass.
			name: "index empty",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES('delete-all')`)
				return err
			},
			expect: "events_fts is empty",
		},
		{
			// Index data lost while its metadata survived — a truncated copy,
			// a bad transfer, bit rot. This is the case that only the MATCH
			// probe catches: the table is present and events_fts_docsize still
			// reports a document per row, so both of the other two checks pass
			// and the artifact looks entirely healthy until something searches.
			name: "index data corrupt",
			mutate: func(db *sql.DB) error {
				_, err := db.Exec(`DELETE FROM events_fts_data`)
				return err
			},
			expect: "events_fts is not queryable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := stageArtifact(t, tc.mutate)
			_, serviceLog, startErr := bootService(t, dir)

			if startErr == nil {
				t.Fatalf("the service booted against an artifact whose FTS index is unusable "+
					"(%s); every /api/search would return an empty result and nothing would "+
					"report it", tc.name)
			}
			if !strings.Contains(serviceLog, tc.expect) {
				t.Errorf("the service refused to boot, but its log does not say why.\n"+
					"want it to contain %q\n--- log ---\n%s", tc.expect, serviceLog)
			}
		})
	}
}

// TestBootProbeAcceptsHealthyArtifact is the control. Without it the test above
// passes just as well against a probe that rejects everything.
func TestBootProbeAcceptsHealthyArtifact(t *testing.T) {
	dir := stageArtifact(t, nil)
	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused an artifact that is not broken: %v\n--- log ---\n%s",
			startErr, serviceLog)
	}

	var health healthDoc
	fetchJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("status: want ok, got %q", health.Status)
	}
	for lang, db := range health.Databases {
		if !db.FTS.Consistent || db.FTS.Indexed != db.Rows {
			t.Errorf("%s: healthy artifact reported as inconsistent (rows=%d indexed=%d consistent=%v)",
				lang, db.Rows, db.FTS.Indexed, db.FTS.Consistent)
		}
	}
}

// TestPartialIndexIsReportedNotFatal covers the middle case, which is the one
// the release check is actually for. A partly-built index is not grounds to
// take the service down — everything except the completeness of search still
// works — so it must boot, say so in the log, and mark itself degraded.
func TestPartialIndexIsReportedNotFatal(t *testing.T) {
	// Drop exactly one row from each index, leaving the row itself in place.
	dir := stageArtifact(t, func(db *sql.DB) error {
		var id int
		var title, description, tags string
		err := db.QueryRow(
			`SELECT id, title, description, tags FROM events ORDER BY id LIMIT 1`,
		).Scan(&id, &title, &description, &tags)
		if err != nil {
			return err
		}
		_, err = db.Exec(
			`INSERT INTO events_fts(events_fts, rowid, title, description, tags) VALUES('delete',?,?,?,?)`,
			id, title, description, tags)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("a partial index took the service down; it should degrade, not die: %v\n"+
			"--- log ---\n%s", startErr, serviceLog)
	}

	var health healthDoc
	fetchJSON(t, base+"/health", &health)

	if health.Status != "degraded" {
		t.Errorf("status: want degraded with an incomplete index, got %q", health.Status)
	}
	for lang, db := range health.Databases {
		if db.FTS.Consistent {
			t.Errorf("%s: reports a consistent index after one document was removed from it", lang)
		}
		if db.FTS.Indexed != db.Rows-1 {
			t.Errorf("%s: indexed=%d, want %d (rows=%d, one removed)",
				lang, db.FTS.Indexed, db.Rows-1, db.Rows)
		}
	}
	if !strings.Contains(serviceLog, "does not cover every row") {
		t.Errorf("nothing in the log warns about the incomplete index:\n%s", serviceLog)
	}
}

// TestIndexedIsNotReadFromTheVirtualTable pins the reason `indexed` comes from
// events_fts_docsize. events_fts is an external-content table, so a count
// against it is answered by reading events — it returns the row count no matter
// what state the index is in, and would report a wholly empty index as
// perfectly consistent.
func TestIndexedIsNotReadFromTheVirtualTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	if err := seedArtifact(path, "en"); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES('delete-all')`); err != nil {
		t.Fatalf("emptying the index: %v", err)
	}

	var rows, viaVirtualTable, viaDocsize int64
	for q, dst := range map[string]*int64{
		`SELECT count(*) FROM events`:             &rows,
		`SELECT count(*) FROM events_fts`:         &viaVirtualTable,
		`SELECT count(*) FROM events_fts_docsize`: &viaDocsize,
	} {
		if err := db.QueryRow(q).Scan(dst); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if viaDocsize != 0 {
		t.Errorf("docsize reports %d documents in an emptied index", viaDocsize)
	}
	if viaVirtualTable != rows {
		t.Logf("count(*) on the external-content table returned %d for %d rows; it no "+
			"longer reads through to the content table. The docsize source stays "+
			"correct either way.", viaVirtualTable, rows)
	}
}

// TestTagsAnswersWithAnArrayWhenThereAreNone pins the shape of the one response
// in this service that could still be JSON null.
//
// /api/tags scans into its slice with Raw().Scan(), which leaves it nil when
// nothing matches, and a nil slice marshals to null rather than []. Search hit
// this and was fixed; /api/categories was written correctly for the same reason;
// this endpoint kept the old shape, so two sibling list endpoints answered
// differently on the same artifact and a client had to special-case one of them.
//
// It takes an artifact where no row carries a usable tag to reach, which is why
// no existing test could: every fixture and every release has tags.
func TestTagsAnswersWithAnArrayWhenThereAreNone(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE events SET tags = '[]'`)
		return err
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact with no tags: %v\n"+
			"--- log ---\n%s", startErr, serviceLog)
	}

	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	if code := getFrom(t, base, "/api/tags?lang=ru", &raw); code != http.StatusOK {
		t.Fatalf("/api/tags: want 200, got %d", code)
	}
	if string(raw.Data) != "[]" {
		t.Errorf("/api/tags: want an empty array, got %s — null is what a nil slice marshals "+
			"to, and a caller iterating the list has to special-case it", raw.Data)
	}

	// The control: nothing about this artifact stops the rest of the service
	// answering, so an empty list here is the real answer rather than a failure
	// that happens to render as one.
	var list eventList
	if code := getFrom(t, base, "/api/events?lang=ru&limit=100", &list); code != http.StatusOK {
		t.Fatalf("/api/events: want 200, got %d", code)
	}
	if list.Pagination.Total != 5 {
		t.Errorf("/api/events total: want 5, got %d", list.Pagination.Total)
	}
}

func fetchJSON(t *testing.T, url string, into interface{}) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
}
