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
//	       created_at, updated_at, url_path, category, landmark)
//
// The column order above is RU's. EN declares the same names in a different
// order — media is fourth in RU and eighth in EN — so nothing here or in the
// tests may be positional; match on names.
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
// Category is the single mandatory classification, one value per row from a
// closed set, and it is what the website colours and filters by. It is a plain
// string rather than a pointer on purpose: absence is a validator failure
// upstream, not a state this API should be able to represent. Note that the
// closed set is enforced by validate.py invariant 13 and not by the DDL — the
// column is declared TEXT with notnull=0 — so the data is clean because the
// publisher checks it, not because the database would refuse otherwise.
// Measured 2026-08-12 across both artifacts: 0 NULL, 0 empty, 0 values outside
// the set, in 1,146 rows.
//
// Deliberately not an enum, and no list of permitted values anywhere in this
// package: canonical owns the vocabulary and it *changes*. It shipped on
// 2026-08-09 with fourteen values, gained `security` the next day, and on
// 2026-08-12 was rewritten down to eight — `bitcoin` and `first` dissolved
// entirely. A validating type here would have had to be rebuilt and redeployed
// twice: once to accept a value the data already contained, and once to stop
// accepting two it no longer does. So this field carries whatever the artifact
// holds, and anything that genuinely needs the current set reads it from the
// artifact.
//
// Consumers used to derive this from tags[0]. That inference is now wrong: tag
// order carries no meaning, and the two vocabularies do not correspond — `first`
// is a tag on 104 RU rows and a category on none, having been a category on 53
// of them until 2026-08-12.
//
// Landmark answers one question — is this event important to a bitcoiner — and
// exists to drive one UI control, a switch that hides everything else. It is
// orthogonal to Category: 402 of 581 RU rows and 394 of 565 EN rows carry it,
// spread across every category.
//
// A plain bool, not a pointer, and that is the deliberate half. The column is
// INTEGER NOT NULL DEFAULT 0 and validator invariant 14 pins it to exactly 0 or
// 1, precisely so that "not a landmark" has one spelling — canonical chose the
// constraint over a nullable column to avoid the defect step28 fixed for media
// and references, where a NULL read as falsy in one consumer and as missing in
// another. A *bool here would put that second spelling straight back into the
// JSON, for a state no published artifact can hold.
//
// The one case a pointer would describe is an artifact predating the column, on
// which every row renders `false` rather than "this artifact cannot say". That
// is the same trade Category makes — it renders "" on those files — and it is
// the right one, because the honest signal is delivered where a caller will
// actually meet it: ?landmark= is refused with a 400 that says the artifact
// predates the column, and /health reports landmark.present false for the
// operator. A null in the payload would be a third channel that every client
// has to branch on and none would.
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
	Category    string     `json:"category" gorm:"column:category"`
	Landmark    bool       `json:"landmark" gorm:"column:landmark"` // INTEGER 0/1; false on an artifact predating the column
	CreatedAt   *time.Time `json:"created_at"`                      // NULL on many rows; renders as null
	UpdatedAt   *time.Time `json:"updated_at"`                      // NULL on many rows; renders as null
}

// hasColumn reports whether the events table in this artifact carries a column.
//
// Asked of the schema rather than inferred from a failed SELECT. Matching on
// "no such column" would work today but puts a driver's error text on the boot
// path, where a reworded message becomes a service that will not start;
// pragma_table_info answers the question directly and is the same idiom the
// test suite already uses to read an artifact's columns.
//
// It exists because two columns now need it. `category` arrived on 2026-08-09
// and `landmark` on 2026-08-12, releases predating each are still on the box as
// rollback targets, and the handlers that name either column by hand fail with
// `no such column` against them — which is a 500 for a question this service
// can otherwise answer perfectly well. See loadCategories and loadLandmark.
//
// The name is a bind parameter in a WHERE clause, not a spliced identifier, so
// there is nothing here to quote.
func hasColumn(db *gorm.DB, name string) (bool, error) {
	var n int64
	if err := db.Raw(
		`SELECT count(*) FROM pragma_table_info('events') WHERE name = ?`, name,
	).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("checking for the %s column: %w", name, err)
	}
	return n > 0, nil
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
