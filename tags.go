package main

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
)

// Structure for the /api/tags response
type TagInfo struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Handler for /api/tags
func getTagsHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)

	zlog.Info().Str("lang", lang).Msg("getTagsHandler called")

	// Initialised, not declared nil, for the reason spelled out in
	// ftsSearchHandler: Raw().Scan() leaves the slice nil when nothing matches
	// and a nil slice marshals to JSON null, so a caller would have to
	// special-case one endpoint. /api/categories was written this way from the
	// start; this one is its sibling and must not answer in a different shape.
	// Reachable only on an artifact where no row carries a usable tag.
	result := []TagInfo{}
	// NOTHING MAY FOLLOW THE FINAL SEMICOLON — not even a comment. SQLite
	// prepares the text after it as a further statement, and a comment-only
	// statement prepares successfully to a NULL handle. go-sqlite3 then steps
	// that NULL handle, gets neither DONE nor ROW, and returns nil instead of
	// io.EOF (sqlite3.go:2238), so database/sql calls Next forever. The
	// handler hangs, holding the connection, with no error and no timeout.
	// This endpoint hung for exactly that reason. Comments anywhere before the
	// final semicolon are fine.
	sqlQuery := `
SELECT
    LOWER(j.value) AS tag,
    -- DISTINCT because this endpoint counts events, not occurrences. Four rows
    -- once listed the same tag twice (RU 160/231 'price', RU 333 and EN 155
    -- 'bitcoin'), so COUNT(*) read 446 for 'bitcoin' against the 445 events
    -- that carried it. Canonical normalised those rows and now rejects
    -- duplicates in validator invariant 6, so the discrepancy should not recur
    -- — but the API is not the right place to depend on that.
    COUNT(DISTINCT e.id) AS count
FROM
    events e,
    json_each(e.tags) j
WHERE
    e.tags IS NOT NULL
    AND e.tags != ''        -- Not an empty string literal
    AND e.tags != '[]'      -- Not an empty JSON array string literal
    AND json_valid(e.tags) = 1 -- Ensures the string is valid JSON
    AND json_type(e.tags) = 'array' -- Ensures it's specifically a JSON array
    AND j.value IS NOT NULL
    AND TRIM(CAST(j.value AS TEXT)) != '' -- Ensures the extracted tag is not an empty or whitespace-only string
GROUP BY
    LOWER(j.value) -- Group by the lowercased tag for case-insensitive counting
-- Order alphabetically by the (now lowercased) tag
ORDER BY
    tag ASC;`
	if err := db.Raw(sqlQuery).Scan(&result).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getTagsHandler: Error executing raw SQL for tags")
		return queryFailed(c, err, "Failed to retrieve tags from database")
	}

	zlog.Info().Int("tag_count", len(result)).Str("lang", lang).Msg("getTagsHandler: Successfully retrieved tags")
	return c.JSON(fiber.Map{"data": result})
}

// Handler for /api/events/tags/:tag
func getEventsByTagHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)
	tagParam := c.Params("tag")

	zlog.Info().Str("tag", tagParam).Str("lang", lang).Msg("getEventsByTagHandler called")

	// The tag filter does not compose with the category or landmark filters.
	// See filterParamRejected.
	if filterParamRejected(c, "category", "landmark") {
		return nil
	}

	page, limit, ok := pagination(c)
	if !ok {
		return nil
	}
	offset := (page - 1) * limit

	var events []Event
	var totalEvents int64

	// Match the tag as a JSON array element, the same way /api/tags counts
	// them, so the two endpoints cannot disagree about what a tag is.
	//
	// This replaces a LIKE '%"tag"%' substring match against the raw JSON. That
	// matched the same rows for every tag in the current vocabulary, but it
	// interpreted LIKE metacharacters in the caller's input: /api/events/tags/%
	// returned all 582 RU events and /api/events/tags/_____ returned 246,
	// because % and _ are wildcards. An equality test inside json_each cannot
	// be steered that way, and it stops depending on how the JSON is spaced or
	// quoted.
	tagMatch := `EXISTS (
		SELECT 1 FROM json_each(events.tags) j
		WHERE json_valid(events.tags) = 1
		  AND json_type(events.tags) = 'array'
		  AND LOWER(j.value) = ?
	)`
	searchTag := strings.ToLower(tagParam)

	countQuery := db.Model(&Event{}).Where(tagMatch, searchTag)
	if err := countQuery.Count(&totalEvents).Error; err != nil {
		zlog.Error().Str("tag", tagParam).Str("lang", lang).Err(err).Msg("getEventsByTagHandler: Failed to count events by tag")
		return queryFailed(c, err, "Failed to count events by tag")
	}

	// Get paginated events matching the tag, newest first. See eventOrder for
	// why the sort does not stop at the date.
	dataQuery := db.Model(&Event{}).Order(eventOrder).Limit(limit).Offset(offset).Where(tagMatch, searchTag)
	if err := dataQuery.Find(&events).Error; err != nil {
		zlog.Error().Str("tag", tagParam).Str("lang", lang).Int("page", page).Int("limit", limit).Err(err).Msg("getEventsByTagHandler: Failed to retrieve events by tag")
		return queryFailed(c, err, "Failed to retrieve events by tag")
	}

	totalPages := (totalEvents + int64(limit) - 1) / int64(limit)
	zlog.Info().Int("event_count", len(events)).Str("tag", tagParam).Str("lang", lang).Int("page", page).Int("limit", limit).Int64("total_matching", totalEvents).Msg("getEventsByTagHandler: Successfully retrieved events")

	return c.JSON(PaginatedEventsResponse{
		Events: events,
		Pagination: PaginationData{
			CurrentPage: page,
			LastPage:    int(totalPages),
			PerPage:     limit,
			Total:       totalEvents,
		},
	})
}
