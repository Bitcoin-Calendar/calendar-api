package main

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

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

	// landmark is the switch the website calls «Только главное»: one boolean,
	// orthogonal to category, hiding everything that is not a landmark. It ANDs
	// with category and with the date filters, so ?category=tech&landmark=true
	// is "the tech events that matter".
	//
	// Presence is checked before the value is parsed, and that order is the
	// point. Against an artifact predating the column — a rollback target —
	// `WHERE landmark = ?` is `no such column: landmark`, measured, which would
	// be a 500 for a question this service can answer perfectly well. It is also
	// the more useful of the two rejections: "this artifact predates the column"
	// tells the caller something "expected true or false" cannot.
	//
	// Unlike category there is no vocabulary to consult, so an unparseable value
	// is the only other way to be wrong. It is a 400 rather than a quiet default
	// for the reason the date filters are: ?landmark=yes silently read as false
	// answers 200 with the 179 rows the caller least wanted, and nothing in the
	// response says the filter was not the one asked for.
	if landmarkStr := c.Query("landmark"); landmarkStr != "" {
		// resolveLang, not the raw parameter, for the reason the category filter
		// spells out: the artifact consulted must be the one this request reads.
		flag := landmarkByLang[resolveLang(lang)]
		if !flag.present {
			return badParam(c, "landmark", landmarkStr, flag.expected())
		}
		// ParseBool rather than a hand-rolled comparison: it is the spelling a Go
		// client would produce and a documented set (1/t/T/TRUE/true/True and the
		// false equivalents), so callers who send "1" are not surprised. The
		// rejection names `true or false` because that is the canonical form to
		// reach for, not because the others are refused.
		want, err := strconv.ParseBool(strings.TrimSpace(landmarkStr))
		if err != nil {
			return badParam(c, "landmark", landmarkStr, flag.expected())
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
