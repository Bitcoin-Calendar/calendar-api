package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

func readFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// trailingCommentAfterFinalSemicolon matches a `--` comment sitting after the
// last semicolon of a statement.
var trailingCommentAfterFinalSemicolon = regexp.MustCompile(`;[^\n]*--`)

// TestNoCommentAfterFinalSemicolon guards a hang that is invisible in review
// and fatal in production.
//
// SQLite prepares whatever follows a statement's semicolon as another
// statement. A fragment containing only a comment prepares successfully, to a
// NULL statement handle. go-sqlite3 then steps that NULL handle, gets back
// neither SQLITE_DONE nor SQLITE_ROW, calls sqlite3_reset and returns nil
// rather than io.EOF (sqlite3.go:2238) — so database/sql believes a row is
// ready, asks for the next one, and loops forever.
//
// The symptom is not an error. The request hangs indefinitely, holding its
// connection, with no log line and no timeout. /api/tags did exactly this in
// both languages.
func TestNoCommentAfterFinalSemicolon(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := readFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(src, "\n") {
			if trailingCommentAfterFinalSemicolon.MatchString(line) {
				t.Errorf("%s:%d: SQL comment after a semicolon will hang the "+
					"query forever, not error:\n\t%s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestTagsHandlerReturns exercises the real handler against the real artifact,
// because the static check above only catches the shape of the mistake and not
// every way of arriving at it.
//
// Skipped unless the artifact fixture is present:
//
//	mkdir -p /tmp/artifact && cp <canonical>/events_{ru,en}.db /tmp/artifact/
func TestTagsHandlerReturns(t *testing.T) {
	const fixture = "/tmp/artifact/events_ru.db"
	if !fileExists(fixture) {
		t.Skipf("fixture %s not present", fixture)
	}

	db, err := InitDB(fixture)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	prevEN, prevRU := DB_EN, DB_RU
	DB_EN, DB_RU = db, db
	t.Cleanup(func() { DB_EN, DB_RU = prevEN, prevRU })

	app := fiber.New()
	app.Get("/api/tags", getTagsHandler)

	req, _ := http.NewRequest(http.MethodGet, "/api/tags?lang=ru", nil)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		res, err := app.Test(req, 10000)
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{code: res.StatusCode}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("/api/tags: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("/api/tags: want 200, got %d", r.code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("/api/tags hung — check for a comment after the final semicolon")
	}
}
