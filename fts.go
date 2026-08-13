package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
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

// Handler for /api/search
func ftsSearchHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)
	query := c.Query("q")

	zlog.Info().Str("query", query).Str("lang", lang).Msg("ftsSearchHandler called")

	if query == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Search query is required"})
	}

	// Search does not narrow by category or landmark. See filterParamRejected.
	if filterParamRejected(c, "category", "landmark") {
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

	// The two columns in the list below that an artifact may genuinely not have.
	// A release predating 2026-08-09 has no `category` and one predating
	// 2026-08-12 has no `landmark`, and those files are rollback targets — so
	// naming either unconditionally makes every search on a rolled-back artifact
	// answer 500, which is the outage the boot path was taught not to cause.
	// Every other handler adapts for free: GORM builds its SELECT from the
	// struct against the table it can see and leaves the field at its zero value
	// — measured against an artifact with both columns dropped — and only this
	// statement enumerates by hand.
	//
	// They are independent rather than a single "old artifact" flag, because the
	// two dates are two months' worth of releases apart and an artifact between
	// them has one column and not the other.
	//
	// Spliced rather than parameterised because a column name is not a bind
	// parameter. The value is built here from literals, never from anything a
	// caller sent. Sprintf is safe on this statement specifically because it
	// contains no other %% verb; anything added below that needs one must escape
	// it or this stops being a formatting string.
	optionalCols := ""
	if categoriesByLang[resolveLang(lang)].present {
		optionalCols += " e.category,"
	}
	if landmarkByLang[resolveLang(lang)].present {
		optionalCols += " e.landmark,"
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
		-- field the SELECT never fetched still marshals to "", null — or, since
		-- landmark, to false, which is why that test had to learn that a false
		-- proves nothing about whether the column was fetched.
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
	`, optionalCols)
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
