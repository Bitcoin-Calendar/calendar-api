package main

import (
	"crypto/subtle" // Added for secure API key comparison
	// Added for parsing JSON tags
	"errors" // Added for gorm.ErrRecordNotFound
	// Added for io.MultiWriter
	// Added for io.MultiWriter
	"log"     // Added for log.Fatal
	"os"      // Added for sorting tags, os.Stdout, os.MkdirAll, os.OpenFile
	"strconv" // Added for pagination
	"strings" // Added for tag processing
	"time"    // Added for rate limiter

	"github.com/gofiber/adaptor/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"    // Added for CORS support
	"github.com/gofiber/fiber/v2/middleware/limiter" // Added for rate limiting
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"gorm.io/gorm" // Added for gorm.ErrRecordNotFound
	// GORM is already used by database.go, no need for direct import here unless using DB functions directly
)

// Define global DB variables for English and Russian databases
var DB_EN *gorm.DB
var DB_RU *gorm.DB

// Helper function to get the correct DB instance based on language
func getDBInstance(langCode string) *gorm.DB {
	if strings.ToLower(langCode) == "ru" {
		return DB_RU
	}
	return DB_EN // Default to English
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
	db := getDBInstance(lang)
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
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to retrieve event",
		})
	}
	zlog.Info().Str("id", id).Str("lang", lang).Msg("getEventHandler: Successfully retrieved event")
	return c.JSON(fiber.Map{"data": event})
}

// Structure for the /api/tags response
type TagInfo struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Handler for /api/tags
func getTagsHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := getDBInstance(lang)

	zlog.Info().Str("lang", lang).Msg("getTagsHandler called")

	var result []TagInfo
	// SQL query to extract, count, and lowercase tags directly from JSON arrays in the 'tags' column.
	// This approach assumes tags are stored as valid JSON arrays (e.g., ["tag1", "tag2"]).
	// It replaces the previous Go-based parsing and aggregation logic.
	// Note: Fallback for comma-separated tags is removed with this SQL-native approach.
	// If tags are not valid JSON arrays, or if individual tags within the array are empty/whitespace-only,
	// they will be ignored by this query.
	sqlQuery := `
SELECT
    LOWER(j.value) AS tag,
    COUNT(*) AS count
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
ORDER BY
    tag ASC; -- Order alphabetically by the (now lowercased) tag
`
	if err := db.Raw(sqlQuery).Scan(&result).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getTagsHandler: Error executing raw SQL for tags")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to retrieve tags from database",
		})
	}

	// Sorting is now handled by the SQL query's "ORDER BY tag ASC".
	// The result slice is already in the correct []TagInfo format.
	zlog.Info().Int("tag_count", len(result)).Str("lang", lang).Msg("getTagsHandler: Successfully retrieved tags")
	return c.JSON(fiber.Map{"data": result})
}

// Handler for /api/events/tags/{tag}
func getEventsByTagHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en") // Default to 'en' if not specified
	db := getDBInstance(lang)
	tagParam := c.Params("tag")
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", "20")

	zlog.Info().Str("tag", tagParam).Str("lang", lang).Str("page", pageStr).Str("limit", limitStr).Msg("getEventsByTagHandler called")

	if tagParam == "" {
		zlog.Warn().Str("lang", lang).Msg("getEventsByTagHandler: Tag parameter is required")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Tag parameter is required",
		})
	}

	// Pagination parameters
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		zlog.Warn().Str("page", pageStr).Str("lang", lang).Str("tag", tagParam).Err(err).Msg("getEventsByTagHandler: Invalid page parameter")
		page = 1
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 1 {
		zlog.Warn().Str("limit", limitStr).Str("lang", lang).Str("tag", tagParam).Err(err).Msg("getEventsByTagHandler: Invalid limit parameter")
		limit = 20
	}
	offset := (page - 1) * limit

	var events []Event
	var totalEvents int64

	// Prepare the search term for LIKE query, expecting tags like ["tag1","searchtag","tag2"]
	// This will search for the tag as a whole word within the JSON array string.
	searchTerm := "%\"" + strings.ToLower(tagParam) + "\"%"

	// Get total count of events matching the tag
	// We need to apply the Where condition for Count as well.
	countQuery := db.Model(&Event{}).Where("LOWER(tags) LIKE ?", searchTerm)
	if err := countQuery.Count(&totalEvents).Error; err != nil {
		zlog.Error().Str("tag", tagParam).Str("lang", lang).Err(err).Msg("getEventsByTagHandler: Failed to count events by tag")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to count events by tag",
		})
	}

	// Get paginated events matching the tag
	// Default sort by date descending
	dataQuery := db.Model(&Event{}).Order("date desc").Limit(limit).Offset(offset).Where("LOWER(tags) LIKE ?", searchTerm)
	if err := dataQuery.Find(&events).Error; err != nil {
		zlog.Error().Str("tag", tagParam).Str("lang", lang).Int("page", page).Int("limit", limit).Err(err).Msg("getEventsByTagHandler: Failed to retrieve events by tag")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to retrieve events by tag",
		})
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
	db := getDBInstance(lang)
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", "20")
	yearStr := c.Query("year")
	monthStr := c.Query("month")
	dayStr := c.Query("day")

	zlog.Info().Str("lang", lang).Str("page", pageStr).Str("limit", limitStr).Str("year", yearStr).Str("month", monthStr).Str("day", dayStr).Msg("getAllEventsHandler called")

	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(limitStr)
	if limit < 1 {
		limit = 20
	}
	offset := (page - 1) * limit

	var events []Event
	var totalEvents int64
	query := db.Model(&Event{})

	// Apply date filters if they are provided
	if yearStr != "" {
		query = query.Where("strftime('%Y', date) = ?", yearStr)
	}
	if monthStr != "" {
		// Ensure month is two-digit ("01"–"12") so that it matches the %m format returned by strftime.
		// Accept both single-digit ("1") and double-digit ("01") inputs.
		if len(monthStr) == 1 {
			monthStr = "0" + monthStr
		}
		query = query.Where("strftime('%m', date) = ?", monthStr)
	}
	if dayStr != "" {
		// Similar padding for day ("01"–"31").
		if len(dayStr) == 1 {
			dayStr = "0" + dayStr
		}
		query = query.Where("strftime('%d', date) = ?", dayStr)
	}

	// First, get the total count of records that match the filter
	if err := query.Count(&totalEvents).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getAllEventsHandler: Failed to count events")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to count events",
		})
	}

	// Then, apply pagination and retrieve the events
	if err := query.Order("date desc").Limit(limit).Offset(offset).Find(&events).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getAllEventsHandler: Failed to retrieve events")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to retrieve events",
		})
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
	db := getDBInstance(lang)
	query := c.Query("q")
	pageStr := c.Query("page", "1")
	limitStr := c.Query("limit", "20")

	zlog.Info().Str("query", query).Str("lang", lang).Msg("ftsSearchHandler called")

	if query == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Search query is required"})
	}

	// Pagination parameters
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 1 {
		limit = 20
	}
	offset := (page - 1) * limit

	var events []Event
	var totalEvents int64

	// Sanitize FTS query
	sanitizedQuery := strings.ReplaceAll(query, "\"", "\"\"")

	countSQL := `
		SELECT COUNT(*)
		FROM events e
		JOIN events_fts fts ON e.id = fts.rowid
		WHERE events_fts MATCH ?;
	`
	if err := db.Raw(countSQL, sanitizedQuery).Scan(&totalEvents).Error; err != nil {
		zlog.Error().Str("query", query).Str("lang", lang).Err(err).Msg("ftsSearchHandler: Failed to count search results")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to count search results"})
	}

	searchSQL := `
		-- "references" is a SQL reserved word: unquoted, SQLite refuses to
		-- parse the statement and every search returns 500.
		SELECT e.id, e.date, e.title, e.description, e.tags, e.media, e."references", e.url_path, fts.rank
		FROM events e
		JOIN events_fts fts ON e.id = fts.rowid
		WHERE events_fts MATCH ?
		ORDER BY fts.rank
		LIMIT ? OFFSET ?;
	`
	if err := db.Raw(searchSQL, sanitizedQuery, limit, offset).Scan(&events).Error; err != nil {
		zlog.Error().Str("query", query).Str("lang", lang).Err(err).Msg("ftsSearchHandler: Failed to execute search")
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to execute search"})
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
		zlog.Info().Str("lang", lang).Str("path", info.Path).Str("sha256", info.SHA256).Int64("rows", info.Rows).Msg("Artifact opened")
	}

	// --- Fiber App Initialization ---
	app := fiber.New()

	// --- Middleware ---
	app.Use(logger.New(logger.Config{
		Output: os.Stdout,
	}))

	app.Use(limiter.New(limiter.Config{
		Max:        100,
		Expiration: 1 * time.Minute,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
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
	api.Get("/events/:id", getEventHandler)
	api.Get("/tags", getTagsHandler)
	api.Get("/events/tags/:tag", getEventsByTagHandler)
	api.Get("/events", getAllEventsHandler)

	// New FTS5 search endpoint, replacing the old /search
	api.Get("/search", ftsSearchHandler)

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	// Set up Fiber app
	app.Static("/", "./docs") // Serve Swagger UI
	log.Fatal(app.Listen(listenAddr()))
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
