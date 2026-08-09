package main

import (
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Event matches the schema defined in Calendar API Spec.md
type Event struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Date        time.Time `json:"date" gorm:"type:date;not null"`
	Title       string    `json:"title" gorm:"size:255;not null"`
	Description string    `json:"description" gorm:"type:text"`
	Tags        string    `json:"tags" gorm:"size:500"`        // JSON array as string
	Media       string    `json:"media" gorm:"type:text"`      // Link to media file(s), stored as a JSON array string e.g., ["url1", "url2"]
	References  string    `json:"references" gorm:"type:text"` // JSON array as string
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Rank        float64   `json:"-" gorm:"-"` // Omit from JSON and DB schema
}

// InitDB opens a database read-only. It performs no schema management of any
// kind: the artifact ships with its own indexes, FTS tables and triggers, and
// recreating them is the failure mode this service is built to prevent. The
// deployed artifact is mode 0444 in a directory the service user cannot write,
// so any DDL here would also be a hard boot failure.
//
// Every part of the DSN matters:
//   - the "file:" prefix is not optional. Without it, mode=ro is silently
//     ignored, and against a writable file the connection will happily create
//     tables.
//   - mode=ro and _query_only=1 block writes at the connection level, so a
//     mistake fails loudly rather than mutating the artifact.
//   - there is deliberately no _journal_mode: switching to WAL is itself a
//     write, and no _synchronous, which only governs fsync on write.
func InitDB(dbPath string) (*gorm.DB, error) {
	localDB, err := gorm.Open(sqlite.Open("file:"+dbPath+"?mode=ro&_query_only=1&_cache_size=10000"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // Or logger.Info for more logs
	})
	if err != nil {
		return nil, err
	}

	return localDB, nil
}
