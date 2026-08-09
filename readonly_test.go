package main

import (
	"os"
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestInitDBOnReadOnlyArtifact reproduces the deployment condition exactly: a
// database file at mode 0444 inside a directory the service user cannot write.
// Anything that issues DDL at startup, or that negotiates WAL, fails here — and
// that is the regression that would actually hurt, because it fails on the box
// and not on a developer's laptop.
func TestInitDBOnReadOnlyArtifact(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events_en.db")

	seedFixture(t, dbPath)

	// Restore write permission before t.TempDir's own cleanup runs, otherwise
	// it cannot unlink the file. Cleanups run LIFO, so this one goes first.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatalf("chmod file: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB on a 0444 database in a 0555 directory: %v", err)
	}

	var rows int64
	if err := db.Model(&Event{}).Count(&rows).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected 1 row, got %d", rows)
	}

	// date must survive as a plain YYYY-MM-DD string, including before the
	// Unix epoch. The driver converts the column to a time.Time on the way
	// out, so without DateString.Scan this reads 1881-09-29T00:00:00Z.
	var event Event
	if err := db.First(&event, 1).Error; err != nil {
		t.Fatalf("read fixture row: %v", err)
	}
	if event.Date != "1881-09-29" {
		t.Fatalf("date: want %q, got %q", "1881-09-29", event.Date)
	}
	if event.URLPath != "/1881-09-29/fixture/" {
		t.Fatalf("url_path: want %q, got %q", "/1881-09-29/fixture/", event.URLPath)
	}
	if event.Media != nil {
		t.Fatalf("absent media: want nil, got %q", *event.Media)
	}

	// No sidecars may have appeared: the directory is not writable, so their
	// presence would mean the open was not read-only.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			t.Fatalf("read-only open created %s sidecar", suffix)
		}
	}
}

// seedFixture writes a minimal events table, mirroring the artifact's columns,
// and closes the connection so nothing is left open against the file.
func seedFixture(t *testing.T, dbPath string) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}

	stmts := []string{
		`CREATE TABLE events (
			id INTEGER PRIMARY KEY,
			-- Declared 'date', exactly as the artifact does. The declared type
			-- is what makes go-sqlite3 hand back a time.Time, so a TEXT column
			-- here would quietly fail to reproduce the real behaviour.
			date date NOT NULL,
			title TEXT NOT NULL,
			description TEXT,
			media TEXT,
			tags TEXT,
			"references" TEXT,
			created_at DATETIME,
			updated_at DATETIME,
			url_path TEXT
		)`,
		`INSERT INTO events (id, date, title, description, media, tags, "references", url_path)
		 VALUES (1, '1881-09-29', 'fixture', 'fixture row', NULL, '["test"]', NULL, '/1881-09-29/fixture/')`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("seed fixture: %v", err)
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("fixture handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
}
