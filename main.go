package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/timeout"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"gorm.io/gorm"
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

// The two open database handles, one per language artifact.
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

// dbFor returns the language's database bound to the request's context, so a
// query cannot outlive the request that asked for it. Handlers must use this
// rather than reach for DB_EN/DB_RU directly: without the context, go-sqlite3
// never checks for cancellation and a runaway query runs until the process
// dies.
func dbFor(c *fiber.Ctx) *gorm.DB {
	db := DB_EN
	if resolveLang(c.Query("lang", "en")) == "ru" {
		db = DB_RU
	}
	return db.WithContext(c.UserContext())
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

// filterParamRejected refuses a filter parameter on an endpoint that does not
// honour it, and reports whether it answered the request — the same shape as
// pagination(), and for the same reason: c.JSON returns nil on success, so a
// helper that returned its error would look like it had refused while the
// handler carried on and overwrote the body under a 400 status line.
//
// Only /api/events honours `category` and `landmark`. The two list endpoints
// this guards — /api/search and /api/events/tags/:tag — accepted `category` and
// ignored it: a client narrowing a search with &category=archives got every
// match, with a 200 and nothing anywhere in the response to say the filter had
// not been applied. That is the same silence the 400 on an unknown category
// exists to break, arrived at from the other side. `landmark` was added to the
// same guard in the same release that added the filter, so it never had a
// window in which it was silently ignored here.
//
// /api/events/:id also ignores both and is deliberately not guarded: a fetch by
// id is not a list a filter could narrow, and the response carries the event's
// actual category and landmark, so nothing about ignoring them is silent.
//
// This deliberately does not become a rule about unknown parameters in general.
// A stray ?foo= carries no expectation that anything will happen; these are real
// parameters of this API with documented meanings, so sending one *is* the
// expectation. Rejecting them here is also the compatible direction: a later
// release that implements a filter turns these 400s into 200s, while a client
// written against a silent pass-through would have to be corrected.
//
// The first parameter present wins. Answering for one is enough to tell the
// caller the request was not honoured as sent, and enumerating the rest would
// only make the message longer than the fix.
func filterParamRejected(c *fiber.Ctx, names ...string) bool {
	for _, name := range names {
		got := c.Query(name)
		if got == "" {
			continue
		}
		zlog.Warn().Str("param", name).Str("got", got).Str("path", c.Path()).
			Msg("rejected a query parameter this endpoint does not honour")
		// The route pattern rather than c.Path(), so /api/events/tags/:tag names
		// itself instead of echoing whichever tag the caller happened to ask for.
		c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fmt.Sprintf(
				"%s is not a filter on %s, so it is refused rather than ignored. "+
					"Only /api/events supports ?%s=", name, c.Route().Path, name),
		})
		return true
	}
	return false
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
	// than a page-size discipline: at 1000 it sits above the corpus — under 600
	// rows in either language — so a caller who asks for it gets everything in
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

// queryFailed renders a failed query. A deadline is returned as an error so the
// timeout middleware can answer 408, which is worth distinguishing from the 500
// that a genuinely broken query earns.
func queryFailed(c *fiber.Ctx, err error, message string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": message})
}

// PaginatedEventsResponse is the body every paginated list endpoint answers
// with.
type PaginatedEventsResponse struct {
	Events     []Event        `json:"events"`
	Pagination PaginationData `json:"pagination"`
}

type PaginationData struct {
	CurrentPage int   `json:"current_page"`
	PerPage     int   `json:"per_page"`
	Total       int64 `json:"total"` // GORM Count returns int64
	LastPage    int   `json:"last_page"`
}

// validAPIKeys holds the accepted keys, parsed from API_KEYS at boot.
var validAPIKeys [][]byte

// authMiddleware checks for a valid API key.
func authMiddleware(c *fiber.Ctx) error {
	providedKey := c.Get("X-API-KEY")
	if providedKey == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "API key required"})
	}

	providedKeyBytes := []byte(providedKey)
	for _, expectedKey := range validAPIKeys {
		if subtle.ConstantTimeCompare(providedKeyBytes, expectedKey) == 1 {
			return c.Next()
		}
	}

	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Invalid API key"})
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

	// --- Landmark flag ---
	// Separate from the loop above rather than folded into it: that one uses
	// `continue` to skip its own logging, which would skip this too.
	//
	// An artifact predating the column is not fatal, for the same reason it is
	// not fatal for category: it is a rollback target, and refusing to start
	// against one turns a rollback into an outage. Only ?landmark= is affected —
	// the field itself still renders, as false.
	for lang, db := range map[string]*gorm.DB{"en": DB_EN, "ru": DB_RU} {
		flag, err := loadLandmark(db)
		if err != nil {
			zlog.Fatal().Str("lang", lang).Err(err).Msg("Failed to read the landmark flag")
		}
		landmarkByLang[lang] = flag
		if !flag.present {
			zlog.Warn().Str("lang", lang).
				Msg("This artifact has no landmark column: it predates 2026-08-12. " +
					"?landmark= will be rejected and every event will report landmark false")
			continue
		}
		// The column is there and no row carries the flag. Unlike an empty
		// category vocabulary this breaks no upstream invariant — validate.py
		// invariant 14 sets no target fraction — but it is still almost
		// certainly a mistake, because it empties the one UI control the column
		// exists to drive, and nothing upstream would say so.
		if flag.count == 0 {
			zlog.Warn().Str("lang", lang).
				Msg("This artifact has a landmark column but no landmarks: no row carries the " +
					"flag. ?landmark=true will answer an empty list")
			continue
		}
		zlog.Info().Str("lang", lang).Int64("landmarks", flag.count).Msg("Landmark flag loaded")
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
		// /health uses.
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

	api.Get("/search", timeout.NewWithContext(ftsSearchHandler, queryTimeout))

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
