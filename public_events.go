package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v2"
	zlog "github.com/rs/zerolog/log"
)

// The public read. One unauthenticated GET that serves the whole of one
// language's artifact as a single document, for a public consumer that cannot
// be handed an API key: the static website fetches it from Node at build time
// (not from a browser, so no CORS origin is involved), and anyone else may read
// it the same way. It is the only route besides /health outside the /api group.
//
// The private list endpoints are built for a bot on loopback: paginated,
// keyed, and carrying media and references exactly as the artifact stores
// them — JSON arrays encoded inside JSON strings, null when absent. That is
// the right contract for a consumer that wants the data untouched; it is the
// wrong one for a page, which wants an array it can iterate on every event.
// So this route decodes those two columns and promises the shape, and it
// promises it unconditionally: a malformed value is normalised to whatever
// usable part it holds rather than dropped, and the document counts the
// fields it could not take as stored so the loss is visible.
//
// The schema name is the version. Anything that changes the shape below is a
// v2 at a new path, not an edit here.
const (
	publicEventsPath   = "/public/v1/events"
	publicEventsSchema = "bitcoin-calendar.public-events.v1"
)

// PublicEventsResponse is the public document.
type PublicEventsResponse struct {
	Schema   string `json:"schema"`
	Language string `json:"language"`
	// Database names the artifact the document was read from in the same two
	// terms /health uses, so a consumer holding both can tell whether it is
	// looking at the release the operator thinks is live.
	Database PublicDatabase `json:"database"`
	// The number of references and media fields that were not a clean JSON
	// array of strings as stored — unparseable, not an array, or an array with
	// entries that are not strings — and were normalised to the strings they
	// held. NULL is not counted: it is the documented spelling of "nothing".
	InvalidReferenceFields int `json:"invalid_reference_fields"`
	InvalidMediaFields     int `json:"invalid_media_fields"`
	// Every event in the artifact, in eventOrder. Never null.
	Events []PublicEvent `json:"events"`
}

type PublicDatabase struct {
	SHA256 string `json:"sha256"`
	Rows   int64  `json:"rows"`
}

// PublicEvent is what a page renders: the private API's id, the core fields
// as stored, and the two optional lists as real arrays. Never null — [] when
// the artifact holds nothing.
//
// Stored strings are passed through as stored. Whether one is a URL worth
// linking is the consumer's judgement, made against the data it can see; a
// route that dropped or repaired a string would make that judgement blind.
type PublicEvent struct {
	ID          uint       `json:"id"`
	Date        DateString `json:"date"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	References  []string   `json:"references"`
	Media       []string   `json:"media"`
}

// stringList decodes one stored media or references value into the array the
// public contract promises, and reports whether it had to leave anything
// behind. The list is never nil, so it marshals as [] rather than null.
func stringList(stored *string) (list []string, invalid bool) {
	list = []string{}
	if stored == nil {
		return list, false
	}
	var items []any
	if err := json.Unmarshal([]byte(*stored), &items); err != nil || items == nil {
		// Not JSON, or JSON that is not an array. Nothing in it can be
		// served; the event itself is unaffected. JSON null decodes to a nil
		// slice without an error, but unlike SQL NULL it is malformed storage.
		return list, true
	}
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			invalid = true
			continue
		}
		list = append(list, s)
	}
	return list, invalid
}

// publicEventsETag derives the document's validator from the four things
// that determine its content: the schema, the build serving it, the language
// actually served, and the artifact's hash. It is strong — two responses
// carrying it are byte-identical — and it changes whenever either a new
// artifact or a new build of this service is released.
//
// The build is not optional. The bytes depend on this code as well as on the
// artifact: how a malformed list is normalised and counted, how a date is
// formatted, how the encoder writes a string. Counting stored JSON null was
// exactly such a change — same artifact, different invalid_*_fields — and
// without the version in the hash a cache holding the old document would have
// been told 304 until the next data release.
func publicEventsETag(lang, artifactSHA256 string) string {
	sum := sha256.Sum256([]byte(publicEventsSchema + "\n" + version + "\n" + lang + "\n" + artifactSHA256))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// ifNoneMatchHas reports whether the header names the given tag. The header
// may carry a comma-separated list, and `*` matches any current
// representation. If-None-Match uses weak comparison even when the current
// validator is strong, per RFC 9110 §13.1.2.
func ifNoneMatchHas(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// publicEventsHandler answers GET /public/v1/events. Registered outside /api,
// beside /health, and needs no API key.
func publicEventsHandler(c *fiber.Ctx) error {
	// resolveLang, not the raw parameter, for the reason every other handler
	// gives: the language in the document must name the artifact actually read,
	// and the ETag must follow the artifact rather than the spelling.
	lang := resolveLang(c.Query("lang", "en"))
	artifact := healthSnapshot.Databases[lang]

	// Revalidation is answered before the query: the tag depends only on the
	// build and startup state, and skipping the work is what the round trip is
	// for.
	etag := publicEventsETag(lang, artifact.SHA256)
	if ifNoneMatchHas(c.Get(fiber.HeaderIfNoneMatch), etag) {
		c.Set(fiber.HeaderETag, etag)
		c.Status(fiber.StatusNotModified)
		return nil
	}

	var events []Event
	if err := dbFor(c).Order(eventOrder).Find(&events).Error; err != nil {
		zlog.Error().Str("lang", lang).Err(err).Msg("publicEventsHandler: Failed to retrieve events")
		return queryFailed(c, err, "Failed to retrieve events")
	}

	doc := PublicEventsResponse{
		Schema:   publicEventsSchema,
		Language: lang,
		// From the snapshot rather than counted again: it is the same file,
		// opened read-only, and /health and this document must not be able to
		// disagree about it.
		Database: PublicDatabase{SHA256: artifact.SHA256, Rows: artifact.Rows},
		Events:   make([]PublicEvent, 0, len(events)),
	}
	for _, e := range events {
		references, badReferences := stringList(e.References)
		if badReferences {
			doc.InvalidReferenceFields++
		}
		media, badMedia := stringList(e.Media)
		if badMedia {
			doc.InvalidMediaFields++
		}
		doc.Events = append(doc.Events, PublicEvent{
			ID:          e.ID,
			Date:        e.Date,
			Title:       e.Title,
			Description: e.Description,
			References:  references,
			Media:       media,
		})
	}
	if doc.InvalidReferenceFields > 0 || doc.InvalidMediaFields > 0 {
		zlog.Warn().Str("lang", lang).
			Int("invalid_reference_fields", doc.InvalidReferenceFields).
			Int("invalid_media_fields", doc.InvalidMediaFields).
			Msg("publicEventsHandler: normalised malformed optional JSON; the artifact should not carry any")
	}

	c.Set(fiber.HeaderETag, etag)
	return c.JSON(doc)
}
