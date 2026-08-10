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
func loadCategories(db *gorm.DB) (categorySet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), categoryProbeTimeout)
	defer cancel()

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

	// An artifact with no categories at all is a real possibility — an older
	// release predating the column would look exactly like this — and it must
	// not be mistaken for "every category is invalid". The handler treats an
	// empty set as "no vocabulary to validate against" and lets the filter
	// through, so this returns cleanly rather than failing the boot: the column
	// is canonical's invariant to enforce, not this service's to refuse to
	// start over.
	set := categorySet{members: make(map[string]bool, len(values)), sorted: values}
	for _, v := range values {
		set.members[v] = true
	}
	return set, nil
}

// known reports whether a category exists in this artifact. An empty
// vocabulary accepts everything: see loadCategories.
func (c categorySet) known(v string) bool {
	if len(c.members) == 0 {
		return true
	}
	return c.members[v]
}

// list renders the vocabulary for an error message.
func (c categorySet) list() string {
	if len(c.sorted) == 0 {
		return "(this artifact carries no categories)"
	}
	s := append([]string(nil), c.sorted...)
	sort.Strings(s)
	return strings.Join(s, ", ")
}
