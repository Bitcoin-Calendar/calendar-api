package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle" // Added for secure API key comparison
	"encoding/hex"
	"errors" // Added for gorm.ErrRecordNotFound
	"fmt"
	"log" // Added for log.Fatal
	"os"
	"os/signal"
	"strconv" // Added for pagination
	"strings" // Added for tag processing
	"syscall"
	"time" // Added for rate limiter

	"github.com/gofiber/adaptor/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"    // Added for CORS support
	"github.com/gofiber/fiber/v2/middleware/limiter" // Added for rate limiting
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/timeout"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"gorm.io/gorm" // Added for gorm.ErrRecordNotFound
)

// Timeouts. Nothing here should take milliseconds, let alone seconds: the
// databases are a few hundred rows and every query is served from a local
// file. These bounds exist so that a query which does misbehave fails visibly
// instead of pinning a connection forever — /api/tags did exactly that, looping
// inside rows.Next() with no error and no deadline to stop it.
const (
	// queryTimeout bounds the work a single request may do. It is enforced
	// through the request's context, which GORM passes to the driver, so it
	// aborts the query itself rather than merely abandoning the caller.
	queryTimeout = 5 * time.Second

	readTimeout  = 10 * time.Second
	writeTimeout = 15 * time.Second // must exceed queryTimeout
	idleTimeout  = 60 * time.Second

	// shutdownTimeout bounds how long a SIGTERM waits for in-flight requests.
	// It must exceed queryTimeout, or a request that is still legitimately
	// working gets cut off by the shutdown rather than by its own deadline.
	shutdownTimeout = 10 * time.Second
)

// Define global DB variables for English and Russian databases
var DB_EN *gorm.DB
var DB_RU *gorm.DB

// resolveLang maps whatever the caller sent to the language that will actually
// serve the request. An unrecognised value falls back to English, which is
// documented behaviour — `lang=xx` is not an error.
//
// Anything keyed by language must resolve through this rather than use the raw
// query parameter, or it ends up describing a different artifact than the one
// being read. That is not hypothetical: ?category= first validated against
// categoriesByLang[<raw lang>], so `?lang=xx&category=nonesuch` found no
// vocabulary to check against, accepted the value, queried the English database
// and answered 200 with an empty list — reintroducing, for one spelling of one
// parameter, exactly the silent-empty-result the 400 exists to prevent.
func resolveLang(langCode string) string {
	if strings.ToLower(langCode) == "ru" {
		return "ru"
	}
	return "en"
}

// Helper function to get the correct DB instance based on language
func getDBInstance(langCode string) *gorm.DB {
	if resolveLang(langCode) == "ru" {
		return DB_RU
	}
	return DB_EN // Default to English
}

// dbFor returns the language's database bound to the request's context, so a
// query cannot outlive the request that asked for it. Handlers must use this
// rather than getDBInstance directly: without the context, go-sqlite3 never
// checks for cancellation and a runaway query runs until the process dies.
func dbFor(c *fiber.Ctx) *gorm.DB {
	return getDBInstance(c.Query("lang", "en")).WithContext(c.UserContext())
}

// isNumericInRange reports whether s is a plain integer within [lo, hi].
// strconv.Atoi is deliberately strict here: it rejects "8.0", "+8" and " 8",
// any of which would otherwise reach strftime and match nothing.
func isNumericInRange(s string, lo, hi int) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= lo && n <= hi
}

// pad2 left-pads a validated 1–2 digit number to the two digits strftime emits.
func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// badParam renders a rejected query parameter. It names the parameter, echoes
// what was sent and says what would be accepted, so the caller can fix it
// without reading the source.
func badParam(c *fiber.Ctx, name, got, want string) error {
	zlog.Warn().Str("param", name).Str("got", got).Msg("rejected query parameter")
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
		"error": fmt.Sprintf("invalid %s %q: expected %s", name, got, want),
	})
}

// categoryParamRejected refuses ?category= on an endpoint that does not filter
// by it, and reports whether it answered the request — the same shape as
// pagination(), and for the same reason: c.JSON returns nil on success, so a
// helper that returned its error would look like it had refused while the
// handler carried on and overwrote the body under a 400 status line.
//
// Only /api/events honours the parameter. The other two endpoints that return
// events accepted it and ignored it: a client narrowing a search with
// &category=bitcoin got every match, with a 200 and nothing anywhere in the
// response to say the filter had not been applied. That is the same silence the
// 400 on an unknown category exists to break, arrived at from the other side.
//
// This deliberately does not become a rule about unknown parameters in general.
// A stray ?foo= carries no expectation that anything will happen; `category` is
// a real parameter of this API with a documented meaning, so sending it *is* the
// expectation. Rejecting it here is also the compatible direction: a later
// release that implements the filter turns these 400s into 200s, while a client
// written against today's silent pass-through would have to be corrected.
func categoryParamRejected(c *fiber.Ctx) bool {
	got := c.Query("category")
	if got == "" {
		return false
	}
	zlog.Warn().Str("param", "category").Str("got", got).Str("path", c.Path()).
		Msg("rejected a query parameter this endpoint does not honour")
	// The route pattern rather than c.Path(), so /api/events/tags/:tag names
	// itself instead of echoing whichever tag the caller happened to ask for.
	c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
		"error": fmt.Sprintf(
			"category is not a filter on %s, so it is refused rather than ignored. "+
				"Only /api/events supports ?category=", c.Route().Path),
	})
	return true
}

// eventOrder is the sort every paginated list endpoint uses: newest first, with
// id breaking ties.
//
// The id is not decoration. SQL guarantees no order among rows the ORDER BY
// cannot separate, and the artifacts have plenty it cannot: 19 dates carry more
// than one event in the English database, several in the Russian. Sorted on the
// date alone, two events on 2017-08-01 may come back in either order, and
// nothing requires the next request to choose the same one — so a caller
// walking pages can be handed one event twice and never shown the other. It
// would read as a bot posting a duplicate and silently dropping an event.
const eventOrder = "date desc, id desc"

// Pagination bounds.
const (
	defaultLimit = 20

	// maxLimit caps one response. It is a backstop against absurd values rather
	// than a page-size discipline: at 1000 it sits above the corpus — 582 rows
	// in the larger language — so a caller who asks for it gets everything in
	// one body, and only a limit that could not be meant seriously is refused.
	// The bound still matters, because without one limit=100000 is accepted as
	// readily as limit=20 and nothing in the response says the endpoint is
	// paginated at all.
	maxLimit = 1000

	// maxPage keeps (page-1)*limit inside an int. Atoi already rejects anything
	// wider than int64, but 9223372036854775807 parses fine and overflows the
	// multiplication into a negative offset, which SQLite silently reads as 0 —
	// page one, wearing the page number the caller asked for. The corpus is
	// under a thousand rows per language, so this bound excludes nothing real.
	maxPage = 1_000_000
)

// pagination parses and validates page and limit for every list endpoint.
//
// Both are validated rather than quietly defaulted, for the reason the date
// filters are: a corrected parameter answers 200 with a plausible body and the
// caller never learns it asked for something else. `page=abc`, `limit=-5` and
// `limit=0` all used to come back as page 1 of 20.
//
// It reports ok=false once it has written the 400 itself. badParam's own return
// value cannot carry that signal — it is c.JSON's error, which is nil on the
// success path — so a handler that tested it for nil would sail on with a zero
// page and a zero limit.
func pagination(c *fiber.Ctx) (page, limit int, ok bool) {
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", strconv.Itoa(defaultLimit))

	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 || page > maxPage {
		badParam(c, "page", pageStr, fmt.Sprintf("1–%d", maxPage))
		return 0, 0, false
	}

	limit, err = strconv.Atoi(limitStr)
	if err != nil || limit < 1 || limit > maxLimit {
		badParam(c, "limit", limitStr, fmt.Sprintf("1–%d", maxLimit))
		return 0, 0, false
	}

	return page, limit, true
}

// ftsSyntaxErrors are the messages SQLite returns when the *caller's* search
// string is not a valid FTS5 expression, as opposed to when something is wrong
// with the server. Measured against the real artifact:
//
//	AND | ) | NOT | ()      fts5: syntax error near "…"
//	*                       unknown special query:
//	" (unbalanced)          unterminated string
//
// The SQL around the MATCH is a fixed string, so the only variable in these
// statements is what the caller sent. Matching on the message is unpleasant —
// the driver offers no error code for this — but the alternative is answering
// 500 to a typo, which makes every 5xx alert untrustworthy.
var ftsSyntaxErrors = []string{
	"fts5: syntax error",
	"unknown special query",
	"unterminated string",
	"malformed MATCH expression",
}

func isFTSSyntaxError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range ftsSyntaxErrors {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// badSearchQuery answers a malformed search expression. It is logged at warn,
// not error: the server is fine, the query was not.
func badSearchQuery(c *fiber.Ctx, query, lang string, err error) error {
	zlog.Warn().Str("query", query).Str("lang", lang).Err(err).Msg("ftsSearchHandler: malformed search query")
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
		"error": "Invalid search query: it is not a valid full-text search expression. " +
			"Bare operators (AND, OR, NOT), unbalanced parentheses or quotes, and a " +
			"leading * are all rejected by SQLite. Quote the text to search for it literally.",
	})
}

// queryFailed renders a failed query. A deadline is returned as an error so the
// timeout middleware can answer 408, which is worth distinguishing from the 500
// that a genuinely broken query earns.
func queryFailed(c *fiber.Ctx, err error, message string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": message})
}

// Define a response structure for paginated events, matching your spec
type PaginatedEventsResponse struct {
	Events     []Event     `json:"events"`     // Changed from Data json:"data"
	Pagination interface{} `json:"pagination"` // Using interface{} for flexibility initially
}

type PaginationData struct {
	CurrentPage int   `json:"current_page"` // Was Page       int   `json:"page"`
	PerPage     int   `json:"per_page"`     // Was Limit      int   `json:"limit"`
	Total       int64 `json:"total"`        // GORM Count returns int64
	LastPage    int   `json:"last_page"`    // Was TotalPages int   `json:"total_pages"`
}

// var expectedAPIKey []byte // Old: single API key
var validAPIKeys [][]byte // New: slice to hold multiple valid API keys

// authMiddleware checks for a valid API key
func authMiddleware(c *fiber.Ctx) error {
	providedKey := c.Get("X-API-KEY")
	if providedKey == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "API key required"})
	}

	providedKeyBytes := []byte(providedKey)
	for _, expectedKey := range validAPIKeys {
		// Securely compare the provided key with each of the expected keys
		if subtle.ConstantTimeCompare(providedKeyBytes, expectedKey) == 1 {
			return c.Next()
		}
	}

	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Invalid API key"})
}

// New handler function for getting a single event
func getEventHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := dbFor(c)
	id := c.Params("id")

	zlog.Info().Str("id", id).Str("lang", lang).Msg("getEventHandler called")

	if id == "" {
		zlog.Warn().Str("lang", lang).Msg("getEventHandler: Event ID is required")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Event ID is required",
		})
	}

	eventID, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		zlog.Warn().Str("id", id).Str("lang", lang).Err(err).Msg("getEventHandler: Invalid Event ID format")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid Event ID format",
		})
	}

	var event Event
	result := db.First(&event, uint(eventID))

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			zlog.Warn().Str("id", id).Str("lang", lang).Err(result.Error).Msg("getEventHandler: Event not found")
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "Event not found",
			})
		}
		zlog.Error().Str("id", id).Str("lang", lang).Err(result.Error).Msg("getEventHandler: Failed to retrieve event")
		return queryFailed(c, result.Error, "Failed to retrieve event")
	}
	zlog.Info().Str("id", id).Str("lang", lang).Msg("getEventHandler: Successfully retrieved event")
	return c.JSON(fiber.Map{"data": event})
}

// Structure for the /api/tags response
type TagInfo struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
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

// Handler for /api/tags
func getTagsHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := dbFor(c)

	zlog.Info().Str("lang", lang).Msg("getTagsHandler called")

	// Initialised, not declared nil, for the reason spelled out in
	// ftsSearchHandler: Raw().Scan() leaves the slice nil when nothing matches
	// and a nil slice marshals to JSON null, so a caller would have to
	// special-case one endpoint. /api/categories was written this way from the
	// start; this one is its sibling and must not answer in a different shape.
	// Reachable only on an artifact where no row carries a usable tag.
	result := []TagInfo{}
	// SQL query to extract, count, and lowercase tags directly from JSON arrays in the 'tags' column.
	// This approach assumes tags are stored as valid JSON arrays (e.g., ["tag1", "tag2"]).
	// It replaces the previous Go-based parsing and aggregation logic.
	// Note: Fallback for comma-separated tags is removed with this SQL-native approach.
	// If tags are not valid JSON arrays, or if individual tags within the array are empty/whitespace-only,
	// they will be ignored by this query.
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

	// Sorting is now handled by the SQL query's "ORDER BY tag ASC".
	// The result slice is already in the correct []TagInfo format.
	zlog.Info().Int("tag_count", len(result)).Str("lang", lang).Msg("getTagsHandler: Successfully retrieved tags")
	return c.JSON(fiber.Map{"data": result})
}

// Handler for /api/events/tags/{tag}
func getEventsByTagHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := dbFor(c)
	tagParam := c.Params("tag")

	zlog.Info().Str("tag", tagParam).Str("lang", lang).Msg("getEventsByTagHandler called")

	if tagParam == "" {
		zlog.Warn().Str("lang", lang).Msg("getEventsByTagHandler: Tag parameter is required")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Tag parameter is required",
		})
	}

	// The tag filter does not compose with the category filter. See
	// categoryParamRejected.
	if categoryParamRejected(c) {
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

	// Get total count of events matching the tag
	// We need to apply the Where condition for Count as well.
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

// Handler for getting all events (replaces the inline function in main)
func getAllEventsHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)
	yearStr := c.Query("year")
	monthStr := c.Query("month")
	dayStr := c.Query("day")

	zlog.Info().Str("lang", lang).Str("year", yearStr).Str("month", monthStr).Str("day", dayStr).Msg("getAllEventsHandler called")

	page, limit, ok := pagination(c)
	if !ok {
		return nil
	}
	offset := (page - 1) * limit

	events := []Event{}
	var totalEvents int64
	query := db.Model(&Event{})

	// Date filters are validated rather than passed through, because an
	// unparseable value here matches nothing and the caller gets 200 with an
	// empty list — identical to a day that genuinely has no events. A bot on
	// that response posts nothing and reports success, so a typo in its date
	// arithmetic would look exactly like a quiet day, indefinitely.
	if yearStr != "" {
		if !isNumericInRange(yearStr, 1000, 9999) {
			return badParam(c, "year", yearStr, "a four-digit year")
		}
		query = query.Where("strftime('%Y', date) = ?", yearStr)
	}
	if monthStr != "" {
		if !isNumericInRange(monthStr, 1, 12) {
			return badParam(c, "month", monthStr, "1–12")
		}
		// Two digits, to match what strftime('%m') returns. Both "1" and "01"
		// are accepted from the caller.
		query = query.Where("strftime('%m', date) = ?", pad2(monthStr))
	}
	if dayStr != "" {
		if !isNumericInRange(dayStr, 1, 31) {
			return badParam(c, "day", dayStr, "1–31")
		}
		query = query.Where("strftime('%d', date) = ?", pad2(dayStr))
	}

	// category is validated against the vocabulary this artifact actually
	// carries, read at boot by loadCategories. An unknown value is a 400 for
	// the same reason a malformed month is: answering 200 with an empty list
	// makes "there is no such category" indistinguishable from "that category
	// has no events", and a client cannot tell a typo from a quiet corner of
	// the corpus.
	//
	// Matched case-insensitively, like the tag filter, and lowercased on the
	// way in because every stored value is lowercase.
	if categoryStr := c.Query("category"); categoryStr != "" {
		want := strings.ToLower(strings.TrimSpace(categoryStr))
		// resolveLang, not the raw parameter: the vocabulary consulted must be
		// the one belonging to the artifact this request will actually read.
		vocab := categoriesByLang[resolveLang(lang)]
		if !vocab.known(want) {
			return badParam(c, "category", categoryStr, vocab.expected())
		}
		// LOWER on the column, not a bare equality: the closed set is enforced
		// by the publisher rather than by the schema, so this must not depend
		// on the stored casing being what it is today.
		query = query.Where("LOWER(TRIM(category)) = ?", want)
	}

	// First, get the total count of records that match the filter
	if err := query.Count(&totalEvents).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getAllEventsHandler: Failed to count events")
		return queryFailed(c, err, "Failed to count events")
	}

	// Then, apply pagination and retrieve the events
	if err := query.Order(eventOrder).Limit(limit).Offset(offset).Find(&events).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getAllEventsHandler: Failed to retrieve events")
		return queryFailed(c, err, "Failed to retrieve events")
	}

	totalPages := (totalEvents + int64(limit) - 1) / int64(limit)

	zlog.Info().Int("event_count", len(events)).Int64("total_matching", totalEvents).Str("lang", lang).Msg("getAllEventsHandler: Successfully retrieved events")

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

// Handler for FTS5 search
func ftsSearchHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := dbFor(c)
	query := c.Query("q")

	zlog.Info().Str("query", query).Str("lang", lang).Msg("ftsSearchHandler called")

	if query == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Search query is required"})
	}

	// Search does not narrow by category. See categoryParamRejected.
	if categoryParamRejected(c) {
		return nil
	}

	page, limit, ok := pagination(c)
	if !ok {
		return nil
	}
	offset := (page - 1) * limit

	// Initialised, not declared nil: this handler scans into the slice with
	// Raw().Scan(), which leaves it nil when nothing matches, and a nil slice
	// marshals to JSON null. Every other endpoint uses Find(), which sets an
	// empty slice, so search was the one place a caller got null instead of []
	// and had to special-case it.
	events := []Event{}
	var totalEvents int64

	// The caller's string is handed to FTS5 as written. It used to be passed
	// through strings.ReplaceAll(query, `"`, `""`), which was not sanitisation —
	// the value is already a bound parameter, so there is no injection to
	// prevent, and MATCH takes an expression rather than a literal. What the
	// doubling actually did was destroy phrase search: `"bitcoin price"` became
	// `""bitcoin price""`, which FTS5 reads as an empty phrase followed by two
	// bare tokens, i.e. an implicit AND. Against the production artifact that
	// answered 39 where the phrase matches 6, and `"price bitcoin"` returned the
	// same 39 rather than its own 23 — word order silently ignored, on the one
	// syntax the documentation tells callers to reach for. It also re-balanced
	// every stray quote, so `bitcoin"` answered 200 instead of the documented
	// 400.
	//
	// Malformed expressions need no pre-filtering here: SQLite rejects them and
	// isFTSSyntaxError turns that into a 400.
	countSQL := `
		SELECT COUNT(*)
		FROM events e
		JOIN events_fts fts ON e.id = fts.rowid
		WHERE events_fts MATCH ?;
	`
	if err := db.Raw(countSQL, query).Scan(&totalEvents).Error; err != nil {
		if isFTSSyntaxError(err) {
			return badSearchQuery(c, query, lang, err)
		}
		zlog.Error().Str("query", query).Str("lang", lang).Err(err).Msg("ftsSearchHandler: Failed to count search results")
		return queryFailed(c, err, "Failed to count search results")
	}

	// The one column in the list below that an artifact may genuinely not have.
	// A release predating 2026-08-09 has no `category`, and those files are
	// rollback targets — so naming it unconditionally makes every search on a
	// rolled-back artifact answer 500, which is the outage the boot path was
	// just taught not to cause. Every other handler adapts for free: GORM builds
	// its SELECT from the struct against the table it can see, and only this
	// statement enumerates by hand.
	//
	// Spliced rather than parameterised because a column name is not a bind
	// parameter. The value is one of two literals decided here, never anything a
	// caller sent. Sprintf is safe on this statement specifically because it
	// contains no other %% verb; anything added below that needs one must escape
	// it or this stops being a formatting string.
	categoryCol := " e.category,"
	if !categoriesByLang[resolveLang(lang)].present {
		categoryCol = ""
	}

	searchSQL := fmt.Sprintf(`
		-- "references" is a SQL reserved word: unquoted, SQLite refuses to
		-- parse the statement and every search returns 500.
		--
		-- This list is the one place in the service that names columns by hand;
		-- every other handler goes through db.Model(&Event{}) and picks up a new
		-- field for free. So this is the statement that silently drops one, and
		-- it had dropped three: category, created_at and updated_at all reached
		-- callers empty while /api/events returned them correctly.
		-- TestEventStructCoversEveryColumn covers search specifically for that
		-- reason, and checks the values rather than the keys, because a struct
		-- field the SELECT never fetched still marshals to "" or null.
		--
		-- fts.rank is deliberately not selected: nothing scans it — there is no
		-- Rank field on Event — and SQLite orders by it perfectly well without
		-- it appearing here. Selecting it only implied a field that does not
		-- exist. Exposing rank as JSON would be a new contract decision: the
		-- value is FTS5-internal, negative, and incomparable across queries.
		SELECT e.id, e.date, e.title, e.description, e.tags, e.media, e."references",
		       e.url_path,%s e.created_at, e.updated_at
		FROM events e
		JOIN events_fts fts ON e.id = fts.rowid
		WHERE events_fts MATCH ?
		-- e.id breaks ties in rank. Without it equally-ranked rows come back in
		-- whatever order the scan produces, which is not required to be the same
		-- order on the next request — so a caller walking pages can be handed one
		-- event twice and never shown another.
		ORDER BY fts.rank, e.id DESC
		LIMIT ? OFFSET ?;
	`, categoryCol)
	if err := db.Raw(searchSQL, query, limit, offset).Scan(&events).Error; err != nil {
		if isFTSSyntaxError(err) {
			return badSearchQuery(c, query, lang, err)
		}
		zlog.Error().Str("query", query).Str("lang", lang).Err(err).Msg("ftsSearchHandler: Failed to execute search")
		return queryFailed(c, err, "Failed to execute search")
	}

	totalPages := (totalEvents + int64(limit) - 1) / int64(limit)

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

func getAllowedOrigins() string {
	v := os.Getenv("CORS_ALLOWED_ORIGINS")
	if v == "" {
		return "http://localhost:3000"
	}
	return v
}

func main() {
	// --- Logger Setup ---
	zlog.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	// --- API Key Setup ---
	apiKeysStr := os.Getenv("API_KEYS")
	if apiKeysStr == "" {
		log.Fatal("API_KEYS environment variable is not set. Authentication is required.")
	}
	keys := strings.Split(apiKeysStr, ",")
	if len(keys) == 0 || (len(keys) == 1 && keys[0] == "") {
		log.Fatal("API_KEYS environment variable is empty or not properly formatted (comma-separated).")
	}
	for _, k := range keys {
		trimmedKey := strings.TrimSpace(k)
		if trimmedKey != "" {
			validAPIKeys = append(validAPIKeys, []byte(trimmedKey))
		}
	}
	if len(validAPIKeys) == 0 {
		log.Fatal("No valid API keys found after parsing API_KEYS. Please check the format.")
	}
	zlog.Info().Int("keys_loaded", len(validAPIKeys)).Msg("API keys loaded")

	// --- Database Initialization for API ---
	// No fallbacks: the databases are an artifact shipped to a path this
	// service is told about. A default would silently point at nothing.
	dbPathEN := os.Getenv("DB_PATH_EN")
	if dbPathEN == "" {
		zlog.Fatal().Msg("DB_PATH_EN environment variable is not set")
	}
	dbPathRU := os.Getenv("DB_PATH_RU")
	if dbPathRU == "" {
		zlog.Fatal().Msg("DB_PATH_RU environment variable is not set")
	}

	var err error
	DB_EN, err = InitDB(dbPathEN)
	if err != nil {
		zlog.Fatal().Err(err).Msg("Failed to initialize English database")
	}
	zlog.Info().Str("db_path", dbPathEN).Msg("English database initialized")

	DB_RU, err = InitDB(dbPathRU)
	if err != nil {
		zlog.Fatal().Err(err).Msg("Failed to initialize Russian database")
	}
	zlog.Info().Str("db_path", dbPathRU).Msg("Russian database initialized")

	// --- Category vocabulary ---
	// Read from the artifacts rather than compiled in, so a category canonical
	// adds works as soon as its data is published rather than after a rebuild
	// and a deploy. See loadCategories.
	//
	// An artifact predating the column is not fatal — it is a rollback target,
	// and refusing to start against one turns a rollback into an outage. It is
	// logged at warn because it does degrade the service, and only ?category=
	// and /api/categories are affected.
	for lang, db := range map[string]*gorm.DB{"en": DB_EN, "ru": DB_RU} {
		set, err := loadCategories(db)
		if err != nil {
			zlog.Fatal().Str("lang", lang).Err(err).Msg("Failed to read the category vocabulary")
		}
		categoriesByLang[lang] = set
		if !set.present {
			zlog.Warn().Str("lang", lang).
				Msg("This artifact has no category column: it predates 2026-08-09. " +
					"?category= will be rejected and /api/categories will be empty")
			continue
		}
		// The column is there and empty. Upstream broke validator invariant 13:
		// no row carries a category, so ?category= can only be rejected. Warn,
		// because an info line reading `categories: 0` is not something anyone
		// reads a boot log to find.
		if len(set.sorted) == 0 {
			zlog.Warn().Str("lang", lang).
				Msg("This artifact has a category column but no categories: no row carries a " +
					"value. ?category= will be rejected and /api/categories will be empty")
			continue
		}
		zlog.Info().Str("lang", lang).Int("categories", len(set.sorted)).Msg("Category vocabulary loaded")
	}

	// --- Health snapshot ---
	healthSnapshot, err = buildHealthSnapshot(map[string]struct {
		Path string
		DB   *gorm.DB
	}{
		"en": {Path: dbPathEN, DB: DB_EN},
		"ru": {Path: dbPathRU, DB: DB_RU},
	})
	if err != nil {
		zlog.Fatal().Err(err).Msg("Failed to build health snapshot")
	}
	for lang, info := range healthSnapshot.Databases {
		zlog.Info().
			Str("lang", lang).
			Str("path", info.Path).
			Str("sha256", info.SHA256).
			Int64("rows", info.Rows).
			Int64("fts_indexed", info.FTS.Indexed).
			Msg("Artifact opened")

		// Not fatal: the service answers every other endpoint correctly, and
		// taking it down would trade partial search for none at all. It is
		// logged at warn and reported by /health so a release check catches it.
		if !info.FTS.Consistent {
			zlog.Warn().
				Str("lang", lang).
				Int64("rows", info.Rows).
				Int64("fts_indexed", info.FTS.Indexed).
				Msg("Full-text index does not cover every row: search will return incomplete results")
		}
	}

	// --- Fiber App Initialization ---
	// Connection-level bounds. These cap a slow or idle peer; they do not stop
	// a handler that is stuck, which is what queryTimeout is for.
	app := fiber.New(fiber.Config{
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	})

	// --- Middleware ---
	app.Use(logger.New(logger.Config{
		Output: os.Stdout,
	}))

	app.Use(limiter.New(limiter.Config{
		Max:        100,
		Expiration: 1 * time.Minute,
		// Key on the API key, not the IP. Every consumer of this service runs
		// on the same box and talks to it over loopback, so c.IP() is 127.0.0.1
		// for all of them and they would share a single 100/min budget — the
		// Telegram bot, the site and anything else starving each other, with
		// the only symptom being intermittent 429s that look like nothing.
		//
		// The key is hashed rather than used directly because this value ends
		// up as a map key in the limiter's store, and a secret does not belong
		// in a data structure that a future dump, metric or log line might
		// expose. Unauthenticated requests fall back to the IP, which is what
		// /health and /metrics use.
		KeyGenerator: func(c *fiber.Ctx) string {
			if k := c.Get("X-API-KEY"); k != "" {
				sum := sha256.Sum256([]byte(k))
				return "key:" + hex.EncodeToString(sum[:8])
			}
			return "ip:" + c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error": "Rate limit exceeded, please try again later.",
			})
		},
	}))

	app.Use(cors.New(cors.Config{
		AllowOrigins:     getAllowedOrigins(),
		AllowMethods:     "GET,HEAD,OPTIONS",
		AllowHeaders:     "X-API-KEY,Content-Type",
		AllowCredentials: false,
	}))

	// Unauthenticated, and outside the /api group on purpose: this is the
	// check the publisher runs after every release.
	app.Get("/health", healthHandler)

	// Setup routes
	api := app.Group("/api", authMiddleware)

	// Read-only endpoints. /api/events answers the date question via the
	// month/day query parameters; there are deliberately no by-date routes.
	// Every handler that touches the database is wrapped so its query carries a
	// deadline. dbFor picks the deadline up from the request context.
	api.Get("/events/:id", timeout.NewWithContext(getEventHandler, queryTimeout))
	api.Get("/tags", timeout.NewWithContext(getTagsHandler, queryTimeout))
	api.Get("/categories", timeout.NewWithContext(getCategoriesHandler, queryTimeout))
	api.Get("/events/tags/:tag", timeout.NewWithContext(getEventsByTagHandler, queryTimeout))
	api.Get("/events", timeout.NewWithContext(getAllEventsHandler, queryTimeout))

	// New FTS5 search endpoint, replacing the old /search
	api.Get("/search", timeout.NewWithContext(ftsSearchHandler, queryTimeout))

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	// Set up Fiber app
	app.Static("/", "./docs") // Serve Swagger UI

	// Listen on its own goroutine so this one can wait for a signal.
	// Listen returns nil once Shutdown has run, so a non-nil error here is a
	// genuine listener failure — a port already in use, most likely.
	go func() {
		if err := app.Listen(listenAddr()); err != nil {
			zlog.Fatal().Err(err).Msg("Listener failed")
		}
	}()

	// publish-db.sh restarts this service on every release, so SIGTERM is a
	// routine event rather than an exceptional one. Without this, systemd's
	// TERM kills the process outright and any request in flight — including one
	// mid-query, which can legitimately take up to queryTimeout — is dropped on
	// the floor as a connection reset.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	zlog.Info().Str("signal", sig.String()).Msg("Shutting down")

	// Longer than queryTimeout, so a request already inside a query gets to
	// finish rather than being cut off a moment before it would have returned.
	if err := app.ShutdownWithTimeout(shutdownTimeout); err != nil {
		zlog.Error().Err(err).Msg("Graceful shutdown failed; exiting anyway")
	}
	zlog.Info().Msg("Stopped")

	// The database handles are deliberately not closed here. They are read-only
	// with nothing buffered to flush, and the process is about to exit.
}

// listenAddr returns the address to bind. It defaults to loopback: this
// service has no public vhost, and a systemd unit cannot narrow a bind the
// app has already widened to 0.0.0.0.
func listenAddr() string {
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:3000"
}
