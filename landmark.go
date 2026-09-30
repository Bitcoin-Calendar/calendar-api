package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
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

// landmarkFilter validates a non-empty ?landmark= value against the artifact
// this request reads and returns the flag asked for. It reports ok=false once
// it has written the 400 itself, for the reason pagination() does.
//
// Presence is checked before the value is parsed, and that order is the point.
// Against an artifact predating the column — a rollback target — `WHERE
// landmark = ?` is `no such column: landmark`, measured, which would be a 500
// for a question this service can answer perfectly well. It is also the more
// useful of the two rejections: "this artifact predates the column" tells the
// caller something "expected true or false" cannot.
//
// Unlike category there is no vocabulary to consult, so an unparseable value
// is the only other way to be wrong. It is a 400 rather than a quiet default
// for the reason the date filters are: ?landmark=yes silently read as false
// answers 200 with the 179 rows the caller least wanted, and nothing in the
// response says the filter was not the one asked for.
func landmarkFilter(c *fiber.Ctx, lang, value string) (want bool, ok bool) {
	// resolveLang, not the raw parameter, for the reason categoryFilter spells
	// out: the artifact consulted must be the one this request reads.
	flag := landmarkByLang[resolveLang(lang)]
	if !flag.present {
		badParam(c, "landmark", value, flag.expected())
		return false, false
	}
	// ParseBool rather than a hand-rolled comparison: it is the spelling a Go
	// client would produce and a documented set (1/t/T/TRUE/true/True and the
	// false equivalents), so callers who send "1" are not surprised. The
	// rejection names `true or false` because that is the canonical form to
	// reach for, not because the others are refused.
	want, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		badParam(c, "landmark", value, flag.expected())
		return false, false
	}
	return want, true
}
