package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// categoryProbeTimeout bounds the boot query, for the same reason probeFTS is
// bounded: a query that hangs here is a boot that hangs, and /api/tags proved
// this service can hang rather than fail.
const categoryProbeTimeout = 10 * time.Second

// categorySet is one language's category vocabulary, read out of its artifact
// at startup.
type categorySet struct {
	// present is whether the artifact has a `category` column at all. It is a
	// different question from whether the vocabulary is empty, and conflating
	// the two is what made an artifact published before 2026-08-09 a boot
	// failure: see loadCategories.
	//
	// The zero value is false, which is also what a lookup for a language that
	// was never loaded returns. That is the safe direction — an unknown
	// language validates against nothing and rejects, rather than accepting
	// silently, which is the shape the ?lang=xx bug took.
	present bool
	// members is the lookup used to validate ?category=. Keys are lowercase.
	members map[string]bool
	// sorted preserves a stable order for error messages, so a client is told
	// what it could have asked for rather than only that it was wrong.
	sorted []string
}

// categoriesByLang holds the vocabulary per language. Populated once by
// loadCategories during boot and read-only afterwards, so no lock is needed:
// the artifact is opened read-only and cannot change under a running process —
// a release replaces the file and restarts the service.
var categoriesByLang = map[string]categorySet{}

// loadCategories reads the distinct categories out of an artifact.
//
// The vocabulary is derived from the data rather than compiled in, and that is
// the whole design decision here. `category` is a closed set, but canonical
// owns it and it grows: the column shipped 2026-08-09 with fourteen values and
// gained `security` the next day. A hardcoded list would have rejected
// `security` with a 400 until a new binary was built *and* deployed — turning a
// content edit into a code release. Reading it at boot means a new category
// works the moment the artifact carrying it is published, which is the same
// moment the rows using it appear.
//
// The cost is one query per artifact at startup, no DDL, no writes.
//
// It is deliberately not refreshed while running. The process holds an open
// file descriptor on a specific inode, so the vocabulary cannot drift under it;
// publish-db.sh restarts the service on every release precisely because that is
// the only way anything here picks up new data.
//
// An artifact that predates the column is not an error. `category` arrived on
// 2026-08-09, every release before that has no such column, and those files are
// still on the box as rollback targets — publish-db.sh's rollback() re-points
// `current` at the previous release and restarts. Treating a missing column as
// a fatal error made this binary refuse to start against any of them, which
// turned a rollback into an outage and silently coupled the binary's version to
// the artifact's. Everything except ?category= works perfectly well on such an
// artifact, so the service boots, says so at warn, and rejects the one filter it
// cannot answer.
func loadCategories(db *gorm.DB) (categorySet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), categoryProbeTimeout)
	defer cancel()

	// Asked of the schema rather than inferred from a failed SELECT. Matching
	// on "no such column" would work today but puts a driver's error text on
	// the boot path, where a reworded message becomes a service that will not
	// start; pragma_table_info answers the question directly and is the same
	// idiom the suite already uses to read an artifact's columns.
	var hasColumn int64
	if err := db.WithContext(ctx).Raw(
		`SELECT count(*) FROM pragma_table_info('events') WHERE name = 'category'`,
	).Scan(&hasColumn).Error; err != nil {
		return categorySet{}, fmt.Errorf("checking for the category column: %w", err)
	}
	if hasColumn == 0 {
		return categorySet{}, nil
	}

	var values []string
	if err := db.WithContext(ctx).Raw(
		// TRIM and LOWER defensively: the column is mandatory by validator
		// invariant 13, not by the DDL, so nothing in the database itself
		// prevents a stray blank or a capital. Measured clean across both
		// artifacts on 2026-08-10, and this costs nothing to keep honest.
		`SELECT DISTINCT LOWER(TRIM(category)) AS category
		 FROM events
		 WHERE category IS NOT NULL AND TRIM(category) != ''
		 ORDER BY category ASC`,
	).Scan(&values).Error; err != nil {
		return categorySet{}, fmt.Errorf("reading the category vocabulary: %w", err)
	}

	// The column exists but carries nothing on any row. That is not a rollback
	// target, it is an upstream failure of validator invariant 13, and the
	// handler treats an empty set as "no vocabulary to validate against" and
	// lets the filter through — which means ?category=anything answers 200 with
	// an empty list. Left as it was, but note that it is now the only route to
	// that behaviour: the case this permissiveness was written for, an artifact
	// predating the column, is handled above and no longer arrives here.
	set := categorySet{present: true, members: make(map[string]bool, len(values)), sorted: values}
	for _, v := range values {
		set.members[v] = true
	}
	return set, nil
}

// known reports whether a category exists in this artifact.
//
// An artifact with no category column knows nothing, so every value is
// rejected: no row can match a column that is not there, and the alternative is
// the empty list that ?category= exists to avoid. An empty vocabulary on an
// artifact that does have the column accepts everything: see loadCategories.
func (c categorySet) known(v string) bool {
	if !c.present {
		return false
	}
	if len(c.members) == 0 {
		return true
	}
	return c.members[v]
}

// expected renders what would have been accepted, as badParam's `want` clause.
func (c categorySet) expected() string {
	if !c.present {
		return "nothing: this artifact predates the category column, so no value can match"
	}
	// Unreachable while known() lets an empty vocabulary through, and kept
	// anyway so that changing that decision does not also need a message.
	if len(c.sorted) == 0 {
		return "nothing: this artifact carries no categories"
	}
	s := append([]string(nil), c.sorted...)
	sort.Strings(s)
	return "one of: " + strings.Join(s, ", ")
}
