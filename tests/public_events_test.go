package tests

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The public read: one unauthenticated GET that hands a consumer the whole of
// one language's artifact as a single document, in a shape that needs no
// second decode. It sits beside /health, outside /api, and is the only route
// that does — everything under /api keeps its key.
//
// It exists for gm-web, whose static build fetches it from Node at build time
// and, being a public client, cannot be handed an API key; other public
// clients may read it the same way, through nginx once it is exposed. The
// private list endpoints are built for a bot on
// loopback: paginated, keyed, and carrying media and references as JSON arrays
// encoded inside JSON strings. None of that is wrong for the bot, and none of
// it is what a page wants.
//
// Only the fields a page renders are promised: the existing integer id,
// date, title, description, and references and media as real arrays of
// strings — [] when the artifact has nothing, never null and never a string.
// Whether a stored string is a URL a page should link to is gm-web's decision;
// this route preserves what the artifact holds and says how many fields it
// could not take verbatim.
const (
	publicEventsPath   = "/public/v1/events"
	publicEventsSchema = "bitcoin-calendar.public-events.v1"
)

// publicEventsDoc mirrors the public envelope. Spelled out here rather than
// imported, for the reason healthDoc is: if the shape changes, this fails to
// decode, and the shape is the contract.
type publicEventsDoc struct {
	Schema                 string         `json:"schema"`
	Language               string         `json:"language"`
	Database               publicDatabase `json:"database"`
	InvalidReferenceFields int            `json:"invalid_reference_fields"`
	InvalidMediaFields     int            `json:"invalid_media_fields"`
	Events                 []publicEvent  `json:"events"`
}

// publicDatabase names the artifact the document was read from, in the two
// terms /health already uses for it, so a consumer holding both can tell
// whether it is looking at the release the operator thinks is live.
type publicDatabase struct {
	SHA256 string `json:"sha256"`
	Rows   int64  `json:"rows"`
}

// publicEvent is the DTO. References and Media are left raw so the assertions
// can tell an array from the two shapes it must never be — null, and the
// private API's array-inside-a-string — rather than having encoding/json
// quietly refuse one and accept the other.
type publicEvent struct {
	ID          int             `json:"id"`
	Date        string          `json:"date"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	References  json.RawMessage `json:"references"`
	Media       json.RawMessage `json:"media"`
}

// publicGet issues an unauthenticated GET — the route's whole premise — and
// hands back the response with its body read, because these tests need the
// headers (ETag, Content-Type) as well as the status and the JSON, which none
// of the existing helpers expose.
func publicGet(t *testing.T, base, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("GET %s: reading body: %v", path, err)
	}
	return res, body
}

// publicEvents fetches the document and fails unless it is served. A 404 here
// is the route not being registered at all; a 401 is it having been put under
// /api by mistake.
func publicEvents(t *testing.T, base, path string) publicEventsDoc {
	t.Helper()
	res, body := publicGet(t, base, path, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s without an API key: want 200, got %d — the public route is "+
			"registered beside /health, outside the /api group, and asks for no key",
			path, res.StatusCode)
	}
	var doc publicEventsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("GET %s: decoding body: %v\n%s", path, err, body)
	}
	return doc
}

// stringArray decodes a references or media value and fails unless it is what
// the public contract promises: a JSON array of strings. null and a string are
// the two shapes the private API emits — null for absence, a string because the
// column stores its array JSON-encoded — and taking both off the consumer's
// hands is the reason this route exists.
func stringArray(t *testing.T, raw json.RawMessage, what string) []string {
	t.Helper()
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "":
		t.Fatalf("%s: the key is missing", what)
	case s == "null":
		t.Fatalf("%s: null — the public route promises an array, [] when the artifact "+
			"holds nothing, so a page can iterate it without a special case", what)
	case strings.HasPrefix(s, `"`):
		t.Fatalf("%s: a string (%s) — that is the private API's array-encoded-as-a-string, "+
			"which the public route exists to decode for the consumer", what, s)
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: not an array of strings: %s (%v)", what, s, err)
	}
	return out
}

// fixtureArray is what the public route must emit for a fixture row's stored
// media or references: [] for NULL, otherwise the stored JSON decoded. The
// fixture's stored values are all clean arrays of strings, so a decode failure
// here is the expectation being wrong, not the service.
func fixtureArray(t *testing.T, stored *string) []string {
	t.Helper()
	if stored == nil {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(*stored), &out); err != nil {
		t.Fatalf("the fixture stores %q, which is not a JSON array of strings: %v", *stored, err)
	}
	return out
}

// eventsByID indexes a document's events, so a test can ask about a specific
// fixture row without depending on the order — which has its own test.
func eventsByID(events []publicEvent) map[int]publicEvent {
	byID := make(map[int]publicEvent, len(events))
	for _, e := range events {
		byID[e.ID] = e
	}
	return byID
}

// TestPublicEventsNeedsNoAPIKey is the premise, and the control that goes with
// it: opening one public read must not have loosened the private surface.
func TestPublicEventsNeedsNoAPIKey(t *testing.T) {
	res, _ := publicGet(t, baseURL, publicEventsPath+"?lang=ru", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s without an API key: want 200, got %d — a public client cannot be "+
			"handed a key, so the route is registered beside /health, outside /api",
			publicEventsPath, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	// /api keeps its key. This is what TestAuthenticationRequired already pins,
	// repeated here so the two halves of the decision are asserted side by side.
	if code := request(t, http.MethodGet, "/api/events?limit=1", false, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /api/events: want 401, got %d — the public route must "+
			"be the only thing outside /api that reads events", code)
	}
}

// TestPublicEventsEnvelope pins the top level: a schema name a consumer can
// dispatch on, the language actually served, and the artifact named in the
// same terms /health uses, so gm-web and the operator can compare notes.
func TestPublicEventsEnvelope(t *testing.T) {
	var health healthDoc
	fetchJSON(t, baseURL+"/health", &health)
	ru, ok := health.Databases["ru"]
	if !ok {
		t.Fatal("/health is missing the ru database; nothing to compare against")
	}

	doc := publicEvents(t, baseURL, publicEventsPath+"?lang=ru")

	if doc.Schema != publicEventsSchema {
		t.Errorf("schema: want %q, got %q — a consumer dispatches on this exact string",
			publicEventsSchema, doc.Schema)
	}
	if doc.Language != "ru" {
		t.Errorf("language: want ru, got %q", doc.Language)
	}

	// The same artifact /health describes, by the same hash. Computed once at
	// startup there; a second read of the file here would be a second answer.
	if doc.Database.SHA256 == "" {
		t.Error("database.sha256 is empty")
	} else if doc.Database.SHA256 != ru.SHA256 {
		t.Errorf("database.sha256: want %s as /health reports, got %s", ru.SHA256, doc.Database.SHA256)
	}
	if doc.Database.Rows != ru.Rows {
		t.Errorf("database.rows: want %d as /health reports, got %d", ru.Rows, doc.Database.Rows)
	}

	// The document is the whole artifact, not a page of it: rows is both what
	// the artifact holds and what the body carries.
	if int64(len(doc.Events)) != doc.Database.Rows {
		t.Errorf("events: %d in the body, database.rows says %d — the public document is "+
			"unpaginated and complete", len(doc.Events), doc.Database.Rows)
	}

	// The fixture's optional JSON is all well-formed, so nothing was normalised.
	if doc.InvalidReferenceFields != 0 || doc.InvalidMediaFields != 0 {
		t.Errorf("invalid_reference_fields=%d invalid_media_fields=%d on a clean fixture, want 0 and 0",
			doc.InvalidReferenceFields, doc.InvalidMediaFields)
	}
}

// TestPublicEventDTO pins the event object: the keys a page renders, the
// existing integer id, the core fields exactly as stored, and references and
// media as arrays in both the shapes the artifact can hold — present, and NULL.
//
// Both shapes must be exercised. The fixture has one row carrying media and
// references and the rest NULL, and the test refuses to pass unless it met
// both, because [] for NULL is the assertion most likely to regress and a
// fixture with no NULL row could not fail it.
func TestPublicEventDTO(t *testing.T) {
	res, body := publicGet(t, baseURL, publicEventsPath+"?lang=ru", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d", publicEventsPath, res.StatusCode)
	}

	// Shape first, on the raw JSON, so a key that is present with the wrong
	// type is reported as that rather than as a decode error.
	var raw struct {
		Events []map[string]json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(raw.Events) == 0 {
		t.Fatal("the document carries no events; nothing to inspect")
	}
	for _, got := range raw.Events {
		for _, key := range []string{"id", "date", "title", "description", "references", "media"} {
			if _, ok := got[key]; !ok {
				t.Errorf("event %s has no %q key", got["id"], key)
			}
		}
		// The private API's id, as a JSON number. A string here would be a new
		// identifier a consumer could not join back to /api/events/:id.
		if _, err := strconv.Atoi(string(got["id"])); err != nil {
			t.Errorf("id %s is not a JSON integer", got["id"])
		}
		stringArray(t, got["references"], fmt.Sprintf("event %s references", got["id"]))
		stringArray(t, got["media"], fmt.Sprintf("event %s media", got["id"]))
	}

	// Then the values, against the fixture the artifact was seeded from.
	var doc publicEventsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	fixture := map[int]fixtureRow{}
	for _, r := range fixtureRows("ru") {
		fixture[r.ID] = r
	}

	sawPresent, sawAbsent := false, false
	for _, e := range doc.Events {
		want, ok := fixture[e.ID]
		if !ok {
			t.Errorf("event %d is not in the fixture", e.ID)
			continue
		}
		// A plain date, as the private API emits it. The column is declared
		// `date`, so this also proves the route went through the same scanner
		// rather than reading the driver's time.Time.
		if e.Date != want.Date {
			t.Errorf("event %d date: want %q, got %q", e.ID, want.Date, e.Date)
		}
		if e.Title != want.Title {
			t.Errorf("event %d title: want %q, got %q", e.ID, want.Title, e.Title)
		}
		if e.Description != want.Description {
			t.Errorf("event %d description: want %q, got %q", e.ID, want.Description, e.Description)
		}

		wantRefs := fixtureArray(t, want.References)
		if got := stringArray(t, e.References, fmt.Sprintf("event %d references", e.ID)); !slices.Equal(got, wantRefs) {
			t.Errorf("event %d references: want %q, got %q", e.ID, wantRefs, got)
		}
		wantMedia := fixtureArray(t, want.Media)
		if got := stringArray(t, e.Media, fmt.Sprintf("event %d media", e.ID)); !slices.Equal(got, wantMedia) {
			t.Errorf("event %d media: want %q, got %q", e.ID, wantMedia, got)
		}

		if want.Media != nil || want.References != nil {
			sawPresent = true
		}
		if want.Media == nil && want.References == nil {
			sawAbsent = true
		}
	}
	if !sawPresent || !sawAbsent {
		t.Fatalf("the fixture no longer carries both a row with media/references and a row "+
			"without (present=%v absent=%v); half of this test cannot fail", sawPresent, sawAbsent)
	}
}

// TestPublicEventsLanguageSemantics keeps the public route on the private
// API's language rules — ru, en, omitted, unknown, case — and pins the part a
// consumer actually depends on: the language in the envelope is the artifact
// the body came from, not the parameter echoed back.
//
// The fixture makes those separable. Event 4 exists only in the RU artifact,
// and the two artifacts hash differently, so a body labelled one way and read
// from the other cannot pass.
func TestPublicEventsLanguageSemantics(t *testing.T) {
	var health healthDoc
	fetchJSON(t, baseURL+"/health", &health)

	for _, tc := range []struct{ query, want string }{
		{"?lang=ru", "ru"},
		{"?lang=RU", "ru"}, // resolveLang lowercases, as it does for /api
		{"?lang=en", "en"},
		{"", "en"},         // omitted defaults to English
		{"?lang=xx", "en"}, // unknown falls back to English; documented, not an error
	} {
		name := tc.query
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			doc := publicEvents(t, baseURL, publicEventsPath+tc.query)
			if doc.Language != tc.want {
				t.Errorf("language: want %q, got %q", tc.want, doc.Language)
			}

			served := health.Databases[tc.want]
			if doc.Database.SHA256 != served.SHA256 {
				t.Errorf("database.sha256: want the %s artifact's %s, got %s",
					tc.want, served.SHA256, doc.Database.SHA256)
			}
			if doc.Database.Rows != served.Rows {
				t.Errorf("database.rows: want %d, got %d", served.Rows, doc.Database.Rows)
			}

			_, hasRUOnlyRow := eventsByID(doc.Events)[4]
			if wantRU := tc.want == "ru"; hasRUOnlyRow != wantRU {
				t.Errorf("carries the RU-only fixture row (id 4): got %v, want %v — the language "+
					"label %q and the artifact the body was read from disagree",
					hasRUOnlyRow, wantRU, doc.Language)
			}
		})
	}
}

// TestPublicEventsOrder pins the list to the private API's order — date
// descending, id descending breaking ties — asserted from the outside, for
// both languages.
//
// The expected order is computed from the fixture rather than written down, so
// it cannot drift from a fixture edit; and the fixture is checked to still
// contain a tie, because without one the tiebreaker half cannot fail.
func TestPublicEventsOrder(t *testing.T) {
	for _, lang := range []string{"ru", "en"} {
		t.Run(lang, func(t *testing.T) {
			rows := fixtureRows(lang)
			perDate := map[string]int{}
			for _, r := range rows {
				perDate[r.Date]++
			}
			tied := false
			for _, n := range perDate {
				if n > 1 {
					tied = true
				}
			}
			if !tied {
				t.Fatalf("no two %s fixture rows share a date; the tiebreaker cannot be proven", lang)
			}

			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Date != rows[j].Date {
					return rows[i].Date > rows[j].Date // YYYY-MM-DD sorts as text
				}
				return rows[i].ID > rows[j].ID
			})
			want := make([]int, len(rows))
			for i, r := range rows {
				want[i] = r.ID
			}

			doc := publicEvents(t, baseURL, publicEventsPath+"?lang="+lang)
			got := make([]int, len(doc.Events))
			for i, e := range doc.Events {
				got[i] = e.ID
			}
			if !slices.Equal(got, want) {
				t.Errorf("order: want %v (date desc, id desc), got %v", want, got)
			}
		})
	}
}

// TestPublicEventsNormalisesMalformedOptionalJSON is the reason the counters
// exist. Canonical validates media and references before publishing, so no
// release should carry a malformed one — but the route's promise is an array
// of strings on every event, and a promise that holds only while upstream
// validation holds is not one a browser can build on.
//
// So each malformed field is normalised to the usable part of what it holds,
// the event keeps its core fields and its place in the list, and the document
// counts the fields it could not take verbatim so the loss is visible. What
// counts as "could not take verbatim" is pinned here: unparseable JSON, valid
// JSON that is not an array, and an array with entries that are not strings.
// NULL is not counted — it is the documented spelling of "nothing", not a
// defect.
//
// The other half of the contract is what is *not* normalised: a stored string
// that is not a URL is passed through exactly as stored and not counted.
// Whether to link it is gm-web's decision, made against the data it sees; a
// route that dropped or repaired it would make that decision blind.
func TestPublicEventsNormalisesMalformedOptionalJSON(t *testing.T) {
	// Ids 1, 2, 3 and 5 exist in both fixtures, so the same mutation produces the
	// same counts in each language. Event 4 (RU only) is left alone.
	dir := stageArtifact(t, func(db *sql.DB) error {
		for _, s := range []string{
			// Not JSON at all.
			`UPDATE events SET "references" = 'not json' WHERE id = 1`,
			// Valid JSON, wrong shape.
			`UPDATE events SET media = '{"not":"an array"}' WHERE id = 3`,
			// An array with entries that are not strings. The strings survive,
			// in order; the rest do not.
			`UPDATE events SET media = '["https://example.org/kept.png", 42, null, {"x":1}, "not a url"]' WHERE id = 5`,
			// Well-formed, and carrying a string that is not a URL. Preserved as
			// stored, not counted.
			`UPDATE events SET "references" = '["https://bitcoin.org/bitcoin.pdf", "not a url"]' WHERE id = 2`,
		} {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})

	base, serviceLog, startErr := bootService(t, dir)
	if startErr != nil {
		t.Fatalf("the service refused to start against an artifact whose optional JSON is "+
			"malformed. Nothing at boot reads those columns, and the private API serves "+
			"them as stored: %v\n--- log ---\n%s", startErr, serviceLog)
	}
	var health healthDoc
	fetchJSON(t, base+"/health", &health)

	for _, lang := range []string{"ru", "en"} {
		t.Run(lang, func(t *testing.T) {
			doc := publicEvents(t, base, publicEventsPath+"?lang="+lang)

			// No row is dropped for the state of an optional column.
			if int64(len(doc.Events)) != health.Databases[lang].Rows {
				t.Errorf("events: %d in the body, the artifact holds %d — a row with malformed "+
					"optional JSON was dropped rather than normalised",
					len(doc.Events), health.Databases[lang].Rows)
			}
			fixture := map[int]fixtureRow{}
			for _, r := range fixtureRows(lang) {
				fixture[r.ID] = r
			}
			byID := eventsByID(doc.Events)
			mustEvent := func(id int) publicEvent {
				t.Helper()
				e, ok := byID[id]
				if !ok {
					t.Fatalf("event %d is missing from the document", id)
				}
				return e
			}
			arrays := func(id int) (refs, media []string) {
				t.Helper()
				e := mustEvent(id)
				return stringArray(t, e.References, fmt.Sprintf("event %d references", id)),
					stringArray(t, e.Media, fmt.Sprintf("event %d media", id))
			}

			// Unparseable references: the event stays whole, the field is [].
			if e := mustEvent(1); e.Date != fixture[1].Date || e.Title != fixture[1].Title || e.Description != fixture[1].Description {
				t.Errorf("event 1 lost its core fields to a malformed references value: %+v", e)
			}
			if refs, media := arrays(1); len(refs) != 0 || len(media) != 0 {
				t.Errorf("event 1: want references [] and media [], got %q and %q", refs, media)
			}

			// A JSON object where an array belongs: [].
			if refs, media := arrays(3); len(refs) != 0 || len(media) != 0 {
				t.Errorf("event 3: want references [] and media [], got %q and %q", refs, media)
			}

			// Non-string entries dropped, strings kept in their stored order, and
			// "not a url" among them — a string is a string at this boundary.
			if _, media := arrays(5); !slices.Equal(media, []string{"https://example.org/kept.png", "not a url"}) {
				t.Errorf("event 5 media: want the two strings in stored order and nothing else, got %q", media)
			}

			// Well-formed and preserved verbatim, including the non-URL.
			refs, media := arrays(2)
			if !slices.Equal(refs, []string{"https://bitcoin.org/bitcoin.pdf", "not a url"}) {
				t.Errorf("event 2 references: want the stored array exactly, got %q — the route "+
					"must not judge or repair a string; gm-web decides what is a URL", refs)
			}
			if !slices.Equal(media, fixtureArray(t, fixture[2].Media)) {
				t.Errorf("event 2 media: want the fixture's stored array, got %q", media)
			}

			// The counters say exactly which fields could not be taken as stored:
			// event 1's references; event 3's and event 5's media. Event 2 is not
			// counted — nothing about it was changed — and neither is any NULL.
			if doc.InvalidReferenceFields != 1 {
				t.Errorf("invalid_reference_fields: want 1 (event 1), got %d", doc.InvalidReferenceFields)
			}
			if doc.InvalidMediaFields != 2 {
				t.Errorf("invalid_media_fields: want 2 (events 3 and 5), got %d", doc.InvalidMediaFields)
			}
		})
	}
}

// SQL NULL is absence, but JSON null is a stored value of the wrong shape.
// Exercise both optional columns so the counters remain independent of which
// field carried the malformed value.
func TestPublicEventsOptionalListShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  *string
		want    []string
		invalid int
	}{
		{name: "SQL NULL", want: []string{}},
		{name: "JSON null", stored: strptr(" null "), want: []string{}, invalid: 1},
		{name: "empty array", stored: strptr("[]"), want: []string{}},
		{name: "object", stored: strptr(`{"url":"https://example.org"}`), want: []string{}, invalid: 1},
		{name: "string", stored: strptr(`"https://example.org"`), want: []string{}, invalid: 1},
		{name: "number", stored: strptr("42"), want: []string{}, invalid: 1},
		{name: "boolean", stored: strptr("true"), want: []string{}, invalid: 1},
		{name: "malformed", stored: strptr(`["unfinished"`), want: []string{}, invalid: 1},
		{name: "mixed array", stored: strptr(`["https://example.org",42,null,false,{},[],"not a url"]`), want: []string{"https://example.org", "not a url"}, invalid: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stageArtifact(t, func(db *sql.DB) error {
				_, err := db.Exec(`UPDATE events SET "references" = ?, media = ? WHERE id = 1`, tc.stored, tc.stored)
				return err
			})
			base, serviceLog, err := bootService(t, dir)
			if err != nil {
				t.Fatalf("boot: %v\n%s", err, serviceLog)
			}
			for _, lang := range []string{"ru", "en"} {
				doc := publicEvents(t, base, publicEventsPath+"?lang="+lang)
				if len(doc.Events) != len(fixtureRows(lang)) || int64(len(doc.Events)) != doc.Database.Rows {
					t.Fatalf("%s: incomplete corpus: %d events, %d reported rows", lang, len(doc.Events), doc.Database.Rows)
				}
				e, ok := eventsByID(doc.Events)[1]
				want := fixtureRows(lang)[0]
				if !ok || e.Date != want.Date || e.Title != want.Title || e.Description != want.Description {
					t.Fatalf("%s: event 1 lost core fields: %+v", lang, e)
				}
				for field, raw := range map[string]json.RawMessage{"references": e.References, "media": e.Media} {
					if got := stringArray(t, raw, field); !slices.Equal(got, tc.want) {
						t.Errorf("%s %s: want %q, got %q", lang, field, tc.want, got)
					}
				}
				if doc.InvalidReferenceFields != tc.invalid || doc.InvalidMediaFields != tc.invalid {
					t.Errorf("%s: want both counters %d, got references=%d media=%d", lang, tc.invalid, doc.InvalidReferenceFields, doc.InvalidMediaFields)
				}
			}
		})
	}
}

// TestPublicEventsETag pins the validator a client or cache revalidates with.
// The document is the whole artifact rendered by one build, and changes only
// when one of those is released, so one strong tag per build, language and
// artifact is both possible and the point: gm-web's build, or a cache in front
// of the route, can ask "still this?" for the cost of a header, and be told 304.
//
// Strong, not weak, because a weak tag says "equivalent" and this document
// has one exact form; two responses with the same tag must be byte-identical.
func TestPublicEventsETag(t *testing.T) {
	bodies := make(map[string][]byte)
	tagFor := func(query string) string {
		t.Helper()
		res, body := publicGet(t, baseURL, publicEventsPath+query, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s%s: want 200, got %d", publicEventsPath, query, res.StatusCode)
		}
		tag := res.Header.Get("ETag")
		if previous, ok := bodies[tag]; ok && !bytes.Equal(previous, body) {
			t.Errorf("same strong ETag %s carried different response bytes", tag)
		}
		bodies[tag] = body
		return tag
	}

	ru := tagFor("?lang=ru")
	if ru == "" {
		t.Fatal("no ETag on the public document; nothing in front of this route can revalidate it")
	}
	if strings.HasPrefix(ru, "W/") {
		t.Errorf("ETag %s is weak; the document has one exact form per language and artifact, so the tag is strong", ru)
	}
	if !strings.HasPrefix(ru, `"`) || !strings.HasSuffix(ru, `"`) {
		t.Errorf("ETag %s is not a quoted entity-tag", ru)
	}

	// Stable: the same language against the same artifact is the same tag.
	if again := tagFor("?lang=ru"); again != ru {
		t.Errorf("ETag changed between two reads of the same document: %s then %s", ru, again)
	}

	// Distinct per language: the two artifacts are different files with
	// different rows, and a shared tag would let a cache serve one as the other.
	en := tagFor("?lang=en")
	if en == "" {
		t.Fatal("no ETag on the English document")
	}
	if en == ru {
		t.Errorf("ru and en carry the same ETag %s; a cache would answer one language's 304 to the other's request", ru)
	}

	// The tag follows the artifact served, not the parameter sent: lang=xx is
	// English, so it is English's tag.
	if xx := tagFor("?lang=xx"); xx != en {
		t.Errorf("lang=xx serves the English artifact but carries ETag %s, not English's %s", xx, en)
	}

	// The revalidation itself.
	res, body := publicGet(t, baseURL, publicEventsPath+"?lang=ru", map[string]string{"If-None-Match": ru})
	if res.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match with the current tag: want 304, got %d", res.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("a 304 carried a %d-byte body; the whole point is not to send the document again", len(body))
	}
	if got := res.Header.Get("ETag"); got != ru {
		t.Errorf("the 304 carries ETag %q, want %s — a cache refreshes its entry from this header", got, ru)
	}

	// And a stale tag is answered in full, or a cache holding an old release
	// would never learn there is a new one.
	res, body = publicGet(t, baseURL, publicEventsPath+"?lang=ru", map[string]string{"If-None-Match": `"not-the-current-tag"`})
	if res.StatusCode != http.StatusOK {
		t.Errorf("If-None-Match with a stale tag: want 200, got %d", res.StatusCode)
	}
	if len(body) == 0 {
		t.Error("a 200 to a stale If-None-Match carried no body")
	}

	for _, tc := range []struct {
		name, header string
		status       int
	}{
		{"weak current", "W/" + ru, http.StatusNotModified},
		{"strong list", `"stale", ` + ru + `, "other"`, http.StatusNotModified},
		{"weak list", `"stale", W/` + ru + `, W/"other"`, http.StatusNotModified},
		{"wildcard", "*", http.StatusNotModified},
		{"stale weak", `W/"stale"`, http.StatusOK},
		{"stale list", `"stale", W/"other"`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := publicGet(t, baseURL, publicEventsPath+"?lang=ru", map[string]string{"If-None-Match": tc.header})
			if res.StatusCode != tc.status {
				t.Errorf("If-None-Match %s: want %d, got %d", tc.header, tc.status, res.StatusCode)
			}
			if got := res.Header.Get("ETag"); got != ru {
				t.Errorf("want current strong ETag %s, got %q", ru, got)
			}
			if tc.status == http.StatusNotModified && len(body) != 0 {
				t.Errorf("304 carried a %d-byte body", len(body))
			}
			if tc.status == http.StatusOK && !bytes.Equal(body, bodies[ru]) {
				t.Error("stale validator did not return the full current document")
			}
		})
	}
}

func TestPublicEventsETagChangesWithArtifact(t *testing.T) {
	dir := stageArtifact(t, func(db *sql.DB) error {
		_, err := db.Exec(`UPDATE events SET title = 'Changed artifact title' WHERE id = 1`)
		return err
	})
	base, serviceLog, err := bootService(t, dir)
	if err != nil {
		t.Fatalf("boot: %v\n%s", err, serviceLog)
	}
	for _, lang := range []string{"ru", "en"} {
		t.Run(lang, func(t *testing.T) {
			path := publicEventsPath + "?lang=" + lang
			before, oldBody := publicGet(t, baseURL, path, nil)
			oldTag := before.Header.Get("ETag")
			if before.StatusCode != http.StatusOK || oldTag == "" {
				t.Fatalf("original artifact: status=%d ETag=%q", before.StatusCode, oldTag)
			}
			after, newBody := publicGet(t, base, path, map[string]string{"If-None-Match": oldTag})
			newTag := after.Header.Get("ETag")
			if after.StatusCode != http.StatusOK || newTag == "" || newTag == oldTag || strings.HasPrefix(newTag, "W/") {
				t.Fatalf("changed artifact must return 200 with a new strong ETag: status=%d old=%q new=%q", after.StatusCode, oldTag, newTag)
			}
			var oldDoc, newDoc publicEventsDoc
			if err := json.Unmarshal(oldBody, &oldDoc); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(newBody, &newDoc); err != nil {
				t.Fatal(err)
			}
			if oldDoc.Language != lang || newDoc.Language != lang || oldDoc.Database.SHA256 == newDoc.Database.SHA256 {
				t.Fatalf("expected different artifacts of the same language: old=%+v new=%+v", oldDoc.Database, newDoc.Database)
			}
			if e := eventsByID(newDoc.Events)[1]; e.Title != "Changed artifact title" {
				t.Errorf("changed artifact event not served: %+v", e)
			}
			res, body := publicGet(t, base, path, map[string]string{"If-None-Match": newTag})
			if res.StatusCode != http.StatusNotModified || len(body) != 0 || res.Header.Get("ETag") != newTag {
				t.Errorf("new artifact revalidation: status=%d body=%d ETag=%q", res.StatusCode, len(body), res.Header.Get("ETag"))
			}
		})
	}
}

// TestPublicEventsETagNamesTheBuild pins the other half of what the tag
// follows. The bytes depend on this service's code as well as on the artifact —
// counting stored JSON null changed invalid_*_fields without any new data — so
// a release of the code alone must change the tag, or a cache holding the old
// document is told 304 until the next data release.
//
// The suite runs one build, so the tag is recomputed from the documented
// inputs, with the version and hash as /health reports them. A tag that
// dropped the version would still pass every test above.
func TestPublicEventsETagNamesTheBuild(t *testing.T) {
	var health healthDoc
	fetchJSON(t, baseURL+"/health", &health)
	if health.Version == "" {
		t.Fatal("/health reports no version; the tag has no build to name")
	}
	derive := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
		return `"` + hex.EncodeToString(sum[:16]) + `"`
	}
	for _, lang := range []string{"ru", "en"} {
		t.Run(lang, func(t *testing.T) {
			res, _ := publicGet(t, baseURL, publicEventsPath+"?lang="+lang, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d", res.StatusCode)
			}
			got := res.Header.Get("ETag")
			sha := health.Databases[lang].SHA256
			if got == derive(publicEventsSchema, lang, sha) {
				t.Fatalf("ETag %s omits the build version: a code-only release would keep the tag "+
					"while changing the bytes", got)
			}
			if want := derive(publicEventsSchema, health.Version, lang, sha); got != want {
				t.Errorf("ETag: want %s from schema, version %q, language and artifact sha256, got %s",
					want, health.Version, got)
			}
		})
	}
}

// TestPublicRoutesRateLimitByIPWhateverTheKey holds the limiter to one budget
// per address on the keyless routes. The limiter keys on X-API-KEY so that
// every loopback consumer gets its own budget, but outside /api nothing checks
// the key — so honouring it there let any caller mint a fresh 100/min bucket
// per request by inventing a header value, on the one keyless route that reads
// the whole corpus.
//
// Its own instance, so the IP bucket starts untouched by the rest of the suite,
// and deltas rather than exhaustion, so nothing after it is left rate-limited.
func TestPublicRoutesRateLimitByIPWhateverTheKey(t *testing.T) {
	base, serviceLog, err := bootService(t, stageArtifact(t, nil))
	if err != nil {
		t.Fatalf("boot: %v\n%s", err, serviceLog)
	}
	remaining := func(path, key string, wantStatus int) int {
		t.Helper()
		headers := map[string]string{}
		if key != "" {
			headers["X-API-KEY"] = key
		}
		res, _ := publicGet(t, base, path, headers)
		if res.StatusCode != wantStatus {
			t.Fatalf("GET %s with key %q: want %d, got %d", path, key, wantStatus, res.StatusCode)
		}
		n, err := strconv.Atoi(res.Header.Get("X-RateLimit-Remaining"))
		if err != nil {
			t.Fatalf("GET %s: X-RateLimit-Remaining = %q: %v", path, res.Header.Get("X-RateLimit-Remaining"), err)
		}
		return n
	}

	// Each keyless-route request, whatever key it carries, draws exactly one
	// from the same bucket. Keyed on the header, the second would start a
	// fresh bucket and read the same number as the first.
	previous := remaining(publicEventsPath, "bogus-key-1", http.StatusOK)
	for _, step := range []struct{ path, key string }{
		{publicEventsPath, "bogus-key-2"},
		{"/health", "bogus-key-3"},
		{publicEventsPath, ""},
		{"/PUBLIC/v1/events", "bogus-key-4"}, // Fiber routes case-insensitively
	} {
		n := remaining(step.path, step.key, http.StatusOK)
		if n != previous-1 {
			t.Errorf("GET %s with key %q: remaining %d after %d, want %d — keyless routes must "+
				"share the caller's IP bucket whatever X-API-KEY says", step.path, step.key, n, previous, previous-1)
		}
		previous = n
	}

	// /api is unchanged: the header still picks the bucket, and a key that is
	// not one of ours is refused. Its request is charged to that key, so the
	// IP bucket moves only by the probe that reads it.
	remaining("/api/events?limit=1", "bogus-key-5", http.StatusUnauthorized)
	if n := remaining(publicEventsPath, "", http.StatusOK); n != previous-1 {
		t.Errorf("a keyed /api request moved the IP bucket: remaining %d after %d, want %d — "+
			"/api must keep budgeting by X-API-KEY", n, previous, previous-1)
	}
}

// TestPublicEventsIsNotAWritePath holds TestNoWriteEndpoints' line on the one
// route that has no key in front of it. The code is asserted exactly, for the
// reason that test gives: a handler that happened to exist and reject an empty
// body would answer 400, and "not 2xx" would let it in without a word. 405 is
// the path rejecting writes; HEAD and CORS preflight remain standard reads.
func TestPublicEventsIsNotAWritePath(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if code := request(t, method, publicEventsPath, false, nil); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: want 405, got %d — the public route is read-only and unauthenticated, "+
				"and this is not a GET, HEAD or CORS preflight", method, publicEventsPath, code)
		}
	}
}

func TestPublicEventsHEADAndPreflight(t *testing.T) {
	getRes, _ := publicGet(t, baseURL, publicEventsPath, nil)
	if getRes.StatusCode != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", getRes.StatusCode)
	}
	for _, tc := range []struct {
		name, method string
		headers      map[string]string
		status       int
	}{
		{"HEAD", http.MethodHead, nil, http.StatusOK},
		{"conditional HEAD", http.MethodHead, map[string]string{"If-None-Match": "W/" + getRes.Header.Get("ETag")}, http.StatusNotModified},
		{"preflight", http.MethodOptions, map[string]string{"Origin": allowedOrigin, "Access-Control-Request-Method": "GET"}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, baseURL+publicEventsPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.status || len(body) != 0 {
				t.Errorf("want %d with no body, got %d with %d bytes", tc.status, res.StatusCode, len(body))
			}
			if tc.method == http.MethodHead && res.Header.Get("ETag") != getRes.Header.Get("ETag") {
				t.Errorf("HEAD ETag %q differs from GET %q", res.Header.Get("ETag"), getRes.Header.Get("ETag"))
			}
			if tc.method == http.MethodOptions && res.Header.Get("Access-Control-Allow-Origin") != allowedOrigin {
				t.Errorf("preflight allowed origin: got %q", res.Header.Get("Access-Control-Allow-Origin"))
			}
		})
	}
}
