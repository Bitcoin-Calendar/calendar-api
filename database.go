package main

import (
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Event matches the schema of the canonical database artifact:
//
//	events(id, date, title, description, media, tags, "references",
//	       created_at, updated_at, url_path)
//
// Date is TEXT in YYYY-MM-DD form and the range starts at 1881-09-29 — before
// the Unix epoch. It is a string here so the API emits "1881-09-29" rather than
// the invented time and timezone of "1881-09-29T00:00:00Z".
//
// Media and References are pointers so that an absent value renders as JSON
// null. Canonical stores NULL and only NULL for these — never "" and never
// "[]" — and "" would claim "an empty media list" rather than "no media".
//
// URLPath is /<date>/<slug>/, the cross-language join key and the website's
// page URL. The Telegram bot already reads it.
type Event struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Date        string    `json:"date" gorm:"type:date;not null"`
	Title       string    `json:"title" gorm:"size:255;not null"`
	Description string    `json:"description" gorm:"type:text"`
	Tags        string    `json:"tags" gorm:"size:500"`        // JSON array as string
	Media       *string   `json:"media" gorm:"type:text"`      // JSON array as string, e.g. ["url1","url2"]; NULL when absent
	References  *string   `json:"references" gorm:"type:text"` // JSON array as string; NULL when absent
	URLPath     string    `json:"url_path" gorm:"column:url_path"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
