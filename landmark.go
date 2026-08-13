package main

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// landmarkProbeTimeout bounds the boot query, for the same reason
// categoryProbeTimeout and probeFTS do: a query that hangs here is a boot that
// hangs, and /api/tags proved this service can hang rather than fail.
const landmarkProbeTimeout = 10 * time.Second

// landmarkSet is what one language's artifact can say about the landmark flag.
//
// Deliberately not a copy of categorySet. `category` is an open-ended closed
// set whose members have to be read out of the data, because a hardcoded list
// goes stale in both directions; `landmark` is one boolean, pinned to 0 or 1 by
// validator invariant 14, and there is no vocabulary to discover. What the two
// genuinely share is the presence question, and that is factored into
// hasColumn rather than duplicated in a second set type.
type landmarkSet struct {
	// present is whether the artifact has a `landmark` column at all — a
	// different question from whether any row carries the flag, and the two
	// have different causes and different fixes. See loadLandmark.
	//
	// The zero value is false, which is also what a lookup for a language that
	// was never loaded returns. That is the safe direction: an unknown language
	// refuses the filter rather than answering it against nothing.
	present bool
	// count is how many rows carry landmark = 1. Reported by /health so a
	// release can see it; the filter itself does not consult it.
	count int64
}

// landmarkByLang holds the flag's state per language. Populated once by
// loadLandmark during boot and read-only afterwards, so no lock is needed: the
// artifact is opened read-only and cannot change under a running process — a
// release replaces the file and restarts the service.
var landmarkByLang = map[string]landmarkSet{}

// loadLandmark reads the landmark flag's state out of an artifact.
//
// An artifact that predates the column is not an error, for exactly the reason
// spelled out at length in loadCategories: `landmark` arrived on 2026-08-12,
// every release before that has no such column, and those files are still on
// the box as rollback targets — publish-db.sh's rollback() re-points `current`
// at the previous release and restarts. Treating a missing column as fatal
// would make this binary refuse to start against any of them, turning a
// rollback into an outage and silently coupling the binary's version to the
// artifact's.
//
// Presence is not cosmetic here. Measured against a real artifact with the
// column dropped: GORM's own statements survive it untouched, because they
// SELECT * and simply leave the struct field at its zero value — but
// `WHERE landmark = ?` fails with `no such column: landmark`, and so would the
// hand-written SELECT in ftsSearchHandler if it named the column
// unconditionally. Both would be a 500 for a question this service can answer
// perfectly well, which is why both consult this.
//
// The count is read for /health rather than for the filter. Unlike the category
// vocabulary, an empty one is not an upstream invariant violation: validate.py
// invariant 14 checks that every value is 0 or 1 and deliberately does not
// check how many are 1, because the flag is an editorial judgement with no
// target fraction. So a column carrying no landmarks at all is legal data — and
// it would still blank the one UI control the flag exists for, with nothing
// upstream to notice. Counting it once at boot is what lets a release see it.
func loadLandmark(db *gorm.DB) (landmarkSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), landmarkProbeTimeout)
	defer cancel()

	present, err := hasColumn(db.WithContext(ctx), "landmark")
	if err != nil {
		return landmarkSet{}, err
	}
	if !present {
		return landmarkSet{}, nil
	}

	var count int64
	if err := db.WithContext(ctx).Raw(
		// `= 1` rather than `!= 0`: the column is NOT NULL in the DDL and
		// invariant 14 admits only 0 and 1, so there is no third state to be
		// generous about. Being generous would also make this count disagree
		// with what the filter matches, which is the one thing /health must not
		// do.
		`SELECT count(*) FROM events WHERE landmark = 1`,
	).Scan(&count).Error; err != nil {
		return landmarkSet{}, fmt.Errorf("counting landmark rows: %w", err)
	}

	return landmarkSet{present: true, count: count}, nil
}

// expected renders what would have been accepted, as badParam's `want` clause.
func (l landmarkSet) expected() string {
	if !l.present {
		return "nothing: this artifact predates the landmark column, so no value can match"
	}
	return "true or false"
}
