package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ftsProbeTimeout bounds the boot probe. A probe that hangs is a boot that
// hangs, and the /api/tags incident showed that a query in this service can in
// fact hang rather than fail.
const ftsProbeTimeout = 10 * time.Second

// ftsProbeTerm is a token deliberately chosen to match nothing in either
// language. The probe cares that the query *executes*, not that it returns
// rows: a term with hits would make the probe's cost scale with the corpus for
// no extra assurance.
const ftsProbeTerm = "zzqxnonexistenttoken"

// FTSHealth reports the state of one artifact's full-text index.
//
// Indexed is read from events_fts_docsize, the shadow table holding one row per
// indexed document. It is deliberately not `SELECT count(*) FROM events_fts`:
// events_fts is an external-content table (content='events'), so a count
// against it is answered by reading the *content* table and therefore always
// equals Rows — it can never reveal a divergence, which is the one thing worth
// measuring here.
type FTSHealth struct {
	Indexed int64 `json:"indexed"`
	// Consistent is Indexed == Rows: every event is reachable by search.
	// When false, search silently returns incomplete results — the failure
	// this field exists to make visible.
	Consistent bool `json:"consistent"`
}

// probeFTS verifies at startup that this artifact's full-text search actually
// works, and returns what /health should report about it.
//
// The build tag guard (fts5_required.go) proves the *binary* can do FTS5. This
// proves the *artifact* can. They are independent failures: a correctly built
// binary opening a database whose FTS tables were dropped, or rebuilt without
// the extension, serves an empty result for every search and logs nothing.
// Search returning zero hits looks identical to a genuine absence of matches,
// so nothing downstream would report it — the Telegram bot would simply go
// quiet. Failing the boot is the only point at which this is loud.
//
// What it cannot check: whether the index contents actually correspond to the
// current rows. FTS5's own integrity-check runs as an INSERT into the table,
// which _query_only=1 correctly refuses. That check belongs to the publisher,
// which runs it against the staged copy before the symlink flip.
func probeFTS(db *gorm.DB, rows int64) (FTSHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ftsProbeTimeout)
	defer cancel()
	db = db.WithContext(ctx)

	// 1. The table exists and is FTS5. COALESCE keeps this to exactly one row
	// whether or not the table is there, so an absent table is an empty string
	// rather than a no-rows condition GORM would report as success.
	var ddl string
	if err := db.Raw(
		`SELECT COALESCE((SELECT sql FROM sqlite_master WHERE type='table' AND name='events_fts'), '')`,
	).Scan(&ddl).Error; err != nil {
		return FTSHealth{}, fmt.Errorf("reading events_fts definition: %w", err)
	}
	if ddl == "" {
		return FTSHealth{}, fmt.Errorf("events_fts is missing: this artifact cannot serve /api/search")
	}
	if !strings.Contains(strings.ToLower(ddl), "fts5") {
		return FTSHealth{}, fmt.Errorf("events_fts is not an FTS5 table: %s", ddl)
	}

	// 2. The production query shape runs. This is the search handler's own
	// count statement, so it exercises the join between events.id and the
	// index's rowid rather than merely touching the index — a content_rowid
	// mismatch would pass a bare MATCH and return nothing here.
	var probe int64
	if err := db.Raw(
		`SELECT count(*) FROM events e JOIN events_fts fts ON e.id = fts.rowid WHERE events_fts MATCH ?`,
		ftsProbeTerm,
	).Scan(&probe).Error; err != nil {
		return FTSHealth{}, fmt.Errorf("events_fts is not queryable: %w", err)
	}

	// 3. How much is actually indexed.
	var indexed int64
	if err := db.Raw(`SELECT count(*) FROM events_fts_docsize`).Scan(&indexed).Error; err != nil {
		return FTSHealth{}, fmt.Errorf(
			"reading events_fts_docsize: %w (an FTS5 table built with columnsize=0 has no such "+
				"shadow table; this service's artifacts are not built that way)", err)
	}
	if indexed == 0 && rows > 0 {
		return FTSHealth{}, fmt.Errorf(
			"events_fts is empty while events holds %d rows: every search would return nothing", rows)
	}

	return FTSHealth{Indexed: indexed, Consistent: indexed == rows}, nil
}
