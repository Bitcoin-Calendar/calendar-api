package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// canonicalEventPath validates the wildcard accepted by /api/events/url/* and
// returns the one representation stored by canonical: /YYYY-MM-DD/slug/.
// Only the outer slashes are optional. An inner slash changes the identity and
// is rejected rather than being cleaned into a different path.
func canonicalEventPath(raw string) (string, bool) {
	trimmed := strings.Trim(raw, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return "", false
	}

	date, slug := parts[0], parts[1]
	if len(date) != len("2006-01-02") || date[4] != '-' || date[7] != '-' {
		return "", false
	}
	for i, r := range date {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", false
	}
	if slug == "" || strings.TrimSpace(slug) != slug || slug == "." || slug == ".." {
		return "", false
	}

	return "/" + date + "/" + slug + "/", true
}

// Handler for /api/events/url/*.
func getEventByURLHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	path, ok := canonicalEventPath(c.Params("*"))
	if !ok {
		zlog.Warn().Str("path", c.Params("*")).Str("lang", lang).
			Msg("getEventByURLHandler: invalid canonical event path")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid event path: expected /YYYY-MM-DD/slug/",
		})
	}

	zlog.Info().Str("path", path).Str("lang", lang).Msg("getEventByURLHandler called")

	var event Event
	result := dbFor(c).Where("url_path = ?", path).First(&event)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			zlog.Warn().Str("path", path).Str("lang", lang).
				Msg("getEventByURLHandler: event not found")
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "Event not found",
			})
		}
		zlog.Error().Str("path", path).Str("lang", lang).Err(result.Error).
			Msg("getEventByURLHandler: failed to retrieve event")
		return queryFailed(c, result.Error, "Failed to retrieve event")
	}

	return c.JSON(fiber.Map{"data": event})
}

// Handler for /api/events/:id
func getEventHandler(c *fiber.Ctx) error {
	lang := c.Query("lang", "en")
	db := dbFor(c)
	id := c.Params("id")

	zlog.Info().Str("id", id).Str("lang", lang).Msg("getEventHandler called")

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

// Handler for /api/events
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
	// carries, read at boot by loadCategories; see categoryFilter. Matched
	// case-insensitively, like the tag filter, and lowercased on the way in
	// because every stored value is lowercase.
	//
	// One value here, as always: repeating the parameter is a search feature,
	// and this endpoint still reads only the first.
	if categoryStr := c.Query("category"); categoryStr != "" {
		want, ok := categoryFilter(c, lang, []string{categoryStr})
		if !ok {
			return nil
		}
		// LOWER on the column, not a bare equality: the closed set is enforced
		// by the publisher rather than by the schema, so this must not depend
		// on the stored casing being what it is today.
		query = query.Where("LOWER(TRIM(category)) = ?", want[0])
	}

	// landmark is the switch the website calls «Только главное»: one boolean,
	// orthogonal to category, hiding everything that is not a landmark. It ANDs
	// with category and with the date filters, so ?category=tech&landmark=true
	// is "the tech events that matter". Validation, and why presence is checked
	// before the value is parsed, is in landmarkFilter.
	if landmarkStr := c.Query("landmark"); landmarkStr != "" {
		want, ok := landmarkFilter(c, lang, landmarkStr)
		if !ok {
			return nil
		}
		// No LOWER/TRIM counterpart here: this column is INTEGER NOT NULL, so
		// unlike category there is no stored casing or padding to defend
		// against. Bound as a Go bool, which the driver binds as 1 or 0.
		query = query.Where("landmark = ?", want)
	}

	if err := query.Count(&totalEvents).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("getAllEventsHandler: Failed to count events")
		return queryFailed(c, err, "Failed to count events")
	}

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
