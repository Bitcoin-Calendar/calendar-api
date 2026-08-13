package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
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
// owns it and it changes in both directions.
//
// It grows: the column shipped 2026-08-09 with fourteen values and gained
// `security` the next day. A hardcoded list would have rejected `security` with
// a 400 until a new binary was built *and* deployed — turning a content edit
// into a code release.
//
// It also shrinks, which the growth case does not cover and which this handles
// only because the set is *derived* rather than merged. On 2026-08-12 canonical
// rewrote the vocabulary from fifteen values to eight, dissolving `bitcoin` and
// `first` across every row. SELECT DISTINCT over live rows means a value that
// leaves the data leaves `members` in the same release, so ?category=bitcoin
// becomes a 400 naming the eight that replaced it — rather than a 200 with an
// empty list, which is what a stale compiled-in list would have produced and is
// exactly the silence this filter exists to break. Nothing survives a release
// to contradict it: the process restarts, and there is no cache.
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

	present, err := hasColumn(db.WithContext(ctx), "category")
	if err != nil {
		return categorySet{}, err
	}
	if !present {
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

	// The column may exist and carry nothing on any row. That is not a rollback
	// target — it is an upstream failure of validator invariant 13 — and it is
	// still not a reason to refuse to boot: every other endpoint answers
	// correctly, exactly as on an artifact predating the column. known() rejects
	// every value in that state, so the filter says so rather than answering 200
	// with an empty list. The caller is told at warn during boot.
	set := categorySet{present: true, members: make(map[string]bool, len(values)), sorted: values}
	for _, v := range values {
		set.members[v] = true
	}
	return set, nil
}

// known reports whether a category exists in this artifact.
//
// An artifact with nothing to check against rejects every value, and there are
// two ways to have nothing: no `category` column at all, or a column no row
// carries a value in. Both leave members empty, and in both no row can match, so
// letting the filter through would answer 200 with an empty list — the outcome
// ?category= validates in order to avoid.
//
// An empty vocabulary used to be accepted instead, on the reasoning that there
// was no vocabulary to validate against. That was written when a missing column
// was the case that reached here; it is handled separately now, and all the
// permissiveness did was leave one route back to the silent empty result.
func (c categorySet) known(v string) bool {
	return c.members[v]
}

// expected renders what would have been accepted, as badParam's `want` clause.
func (c categorySet) expected() string {
	if !c.present {
		return "nothing: this artifact predates the category column, so no value can match"
	}
	// The column is there but no row carries a value. known() rejects
	// everything in that state, so this is what such a caller is told.
	if len(c.sorted) == 0 {
		return "nothing: this artifact carries no categories"
	}
	s := append([]string(nil), c.sorted...)
	sort.Strings(s)
	return "one of: " + strings.Join(s, ", ")
}

// Structure for the /api/categories response
type CategoryInfo struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// Handler for /api/categories — the counterpart to /api/tags, and what a client
// needs to build a filter UI without fetching every event to discover what
// exists.
//
// Simpler than getTagsHandler in one way that matters: tags are a JSON array,
// so that query joins through json_each and must COUNT(DISTINCT e.id) to avoid
// counting a row twice when it lists a tag twice. category is exactly one value
// per row, so a plain COUNT(*) is already an event count and cannot disagree
// with /api/events?category=.
func getCategoriesHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)

	zlog.Info().Str("lang", lang).Msg("getCategoriesHandler called")

	// An artifact predating the category column has nothing to report, and the
	// statement below would fail against it with `no such column`. Answering the
	// empty list is the truth — this artifact carries no categories — and a 500
	// on an endpoint the service can perfectly well answer is not.
	if !categoriesByLang[resolveLang(lang)].present {
		return c.JSON(fiber.Map{"data": []CategoryInfo{}})
	}

	// Initialised, not declared nil, for the reason ftsSearchHandler spells out:
	// Raw().Scan() leaves the slice nil when nothing matches and a nil slice
	// marshals to JSON null, so a caller would get null from one branch of this
	// handler and [] from the other.
	result := []CategoryInfo{}
	// NOTHING MAY FOLLOW THE FINAL SEMICOLON — see getTagsHandler for what a
	// trailing comment does to this driver. It is not a style rule.
	sqlQuery := `
SELECT
    LOWER(TRIM(category)) AS category,
    COUNT(*) AS count
FROM
    events
WHERE
    category IS NOT NULL
    AND TRIM(category) != ''
GROUP BY
    LOWER(TRIM(category))
ORDER BY
    category ASC;`
	if err := db.Raw(sqlQuery).Scan(&result).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getCategoriesHandler: Error executing raw SQL for categories")
		return queryFailed(c, err, "Failed to retrieve categories from database")
	}

	zlog.Info().Int("category_count", len(result)).Str("lang", lang).Msg("getCategoriesHandler: Successfully retrieved categories")
	return c.JSON(fiber.Map{"data": result})
}
