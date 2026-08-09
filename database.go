package main

import (
	"database/sql/driver"
	"fmt"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DateString is the events.date column: YYYY-MM-DD text, from 1881-09-29
// onward. Its JSON form is a plain string.
//
// The scanner is not optional. The column's *declared* type in the artifact is
// `date`, and mattn/go-sqlite3 converts any column declared date/datetime/
// timestamp into a time.Time inside the driver, before GORM sees the value —
// so declaring the field as a plain string is not enough on its own, and the
// API goes on emitting "1881-09-29T00:00:00Z". Scanning back down to the date
// component is what actually fixes the contract. There is no DSN or config
// switch for the driver's behaviour — it is an unconditional switch on the
// declared type — so the alternatives are this or redeclaring the column TEXT,
// which needs a full SQLite table rebuild of the canonical artifact.
//
// One inherited caveat: when the driver cannot parse the stored text it
// returns the zero time silently rather than an error, which would surface
// here as "0001-01-01". That is covered upstream — validate.py invariant 1
// checks every row's date against YYYY-MM-DD, and the publisher runs the
// validator against the staged copy before the symlink flip.
type DateString string

// Scan implements sql.Scanner.
func (d *DateString) Scan(value interface{}) error {
	switch v := value.(type) {
	case nil:
		*d = ""
	case time.Time:
		*d = DateString(v.Format("2006-01-02"))
	case string:
		*d = DateString(v)
	case []byte:
		*d = DateString(v)
	default:
		return fmt.Errorf("events.date: cannot scan %T into DateString", value)
	}
	return nil
}

// Value implements driver.Valuer. This service never writes, but GORM expects
// the pair.
func (d DateString) Value() (driver.Value, error) { return string(d), nil }

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
//
// CreatedAt and UpdatedAt are pointers for the same reason as Media: they are
// genuinely absent on many rows (505 of 582 RU created_at, 265 of 565 EN), and
// a plain time.Time renders those as "0001-01-01T00:00:00Z" — an invented
// timestamp of exactly the kind Date was changed to stop emitting. Their
// declared `datetime` type is correct, though, so unlike Date they need no
// scanner: where a value exists it really is a timestamp.
type Event struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	Date        DateString `json:"date" gorm:"type:date;not null"`
	Title       string     `json:"title" gorm:"size:255;not null"`
	Description string     `json:"description" gorm:"type:text"`
	Tags        string     `json:"tags" gorm:"size:500"`        // JSON array as string
	Media       *string    `json:"media" gorm:"type:text"`      // JSON array as string, e.g. ["url1","url2"]; NULL when absent
	References  *string    `json:"references" gorm:"type:text"` // JSON array as string; NULL when absent
	URLPath     string     `json:"url_path" gorm:"column:url_path"`
	CreatedAt   *time.Time `json:"created_at"` // NULL on many rows; renders as null
	UpdatedAt   *time.Time `json:"updated_at"` // NULL on many rows; renders as null
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
