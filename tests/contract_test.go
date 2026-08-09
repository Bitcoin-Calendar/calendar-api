package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestSearchRejectsMalformedQueries pins that a bad *query* is a client error.
//
// Each of these is a valid thing for a user to type into a search box and an
// invalid FTS5 expression. They used to answer 500, which is a lie: the server
// is fine. It also poisons the signal — once a typo can produce a 5xx, no alert
// on 5xx rates can be trusted.
func TestSearchRejectsMalformedQueries(t *testing.T) {
	// Confirmed against the real artifact; each maps to one of the three
	// distinct SQLite messages the handler matches on.
	malformed := []struct{ query, why string }{
		{"AND", `fts5: syntax error near "AND"`},
		{"NOT", `fts5: syntax error near "NOT"`},
		{")", `fts5: syntax error near ")"`},
		{"()", `fts5: syntax error near ")"`},
		{"a OR", `fts5: syntax error near ""`},
		{"a AND AND b", `fts5: syntax error near "AND"`},
		{"*", "unknown special query"},
		{"^", `fts5: syntax error near ""`},
		// An odd number of quotes. These answered 200 for as long as the handler
		// doubled every quote before handing the string to FTS5, because the
		// doubling re-balanced them into an empty phrase — so the one malformed
		// input a user is most likely to produce by hand was also the one the
		// documentation wrongly promised was rejected.
		{`bitcoin"`, "unterminated string"},
		{`"bitcoin`, "unterminated string"},
		{`a"b`, "unterminated string"},
	}

	for _, tc := range malformed {
		t.Run(tc.query, func(t *testing.T) {
			var body map[string]interface{}
			code := get(t, "/api/search?lang=ru&q="+url.QueryEscape(tc.query), &body)
			if code != http.StatusBadRequest {
				t.Errorf("q=%q returned %d, want 400 (SQLite says: %s)", tc.query, code, tc.why)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Errorf("q=%q: 400 with no error message", tc.query)
			}
		})
	}
}

// TestSearchStillAcceptsValidSyntax is the control. Rejecting everything would
// satisfy the test above; these must keep working, and the prefix query in
// particular is a real feature with a known answer.
func TestSearchStillAcceptsValidSyntax(t *testing.T) {
	valid := []string{"bitcoin", "NEAR", `"a phrase"`, "bitcoi*", "bitcoin OR price"}
	for _, q := range valid {
		t.Run(q, func(t *testing.T) {
			var body map[string]interface{}
			if code := get(t, "/api/search?lang=en&q="+url.QueryEscape(q), &body); code != http.StatusOK {
				t.Errorf("q=%q returned %d, want 200 — a valid expression was rejected", q, code)
			}
		})
	}
}

// TestSearchHonoursPhraseQueries is the regression for a bug that returned 200
// and a plausible answer, which is why nothing caught it for so long.
//
// The handler used to run strings.ReplaceAll(q, `"`, `""`) over the caller's
// string, labelled as sanitisation. The value was already a bound parameter, so
// there was nothing to sanitise; what the doubling did was turn `"a b"` into
// `""a b""` — an empty phrase followed by two bare tokens, which FTS5 evaluates
// as an implicit AND. Quoting therefore did nothing at all, and word order was
// silently ignored, on precisely the syntax the README and the 400 message both
// tell callers to reach for. Against the production English artifact,
// `"bitcoin price"` answered 39 where the phrase matches 6, and `"price
// bitcoin"` answered the same 39 rather than its own 23.
//
// Reversing the word order is what makes this test bite: an implicit AND cannot
// tell the two apart, and a phrase must.
func TestSearchHonoursPhraseQueries(t *testing.T) {
	total := func(q string) int {
		t.Helper()
		var list eventList
		if code := get(t, "/api/search?lang=en&q="+url.QueryEscape(q), &list); code != http.StatusOK {
			t.Fatalf("q=%q returned %d, want 200", q, code)
		}
		return list.Pagination.Total
	}

	// Event 2's title is "Bitcoin whitepaper published", so the two words are
	// adjacent in that order and in no other row.
	const (
		bothWords    = "whitepaper published"
		asPhrase     = `"whitepaper published"`
		reversed     = `"published whitepaper"`
		wantMatching = 1
	)

	// The control: if the terms matched nothing, every assertion below would
	// hold at zero whether or not quoting works.
	if n := total(bothWords); n != wantMatching {
		t.Fatalf("q=%q matched %d events, want %d — the fixture no longer supports this test",
			bothWords, n, wantMatching)
	}

	if n := total(asPhrase); n != wantMatching {
		t.Errorf("q=%s matched %d events, want %d: the words are adjacent in that "+
			"order in event 2's title", asPhrase, n, wantMatching)
	}

	if n := total(reversed); n != 0 {
		t.Errorf("q=%s matched %d events, want 0: no row carries those two words "+
			"in that order, so matching any means the quotes were discarded and "+
			"the query degraded to an implicit AND", reversed, n)
	}
}

// TestEventsArrayIsNeverNull pins the JSON shape across every endpoint that
// returns a list. Search scanned into a nil slice and marshalled it as null,
// so a consumer iterating .events had to special-case exactly one endpoint.
func TestEventsArrayIsNeverNull(t *testing.T) {
	// Each of these matches nothing, which is when the difference shows.
	paths := []string{
		"/api/search?lang=ru&q=zzzznotathing",
		"/api/events?lang=ru&month=1&day=1&page=9999",
		"/api/events/tags/zzzznotatag?lang=ru",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			var raw json.RawMessage
			if code := get(t, p, &raw); code != http.StatusOK {
				t.Fatalf("%s returned %d", p, code)
			}
			var body struct {
				Events json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decoding %s: %v", p, err)
			}
			if string(body.Events) == "null" {
				t.Errorf("%s returned \"events\": null; every list endpoint must "+
					"return [] so callers need no special case", p)
			}
			var events []interface{}
			if err := json.Unmarshal(body.Events, &events); err != nil {
				t.Errorf("%s: events is not an array: %s", p, body.Events)
			}
		})
	}
}

// TestMalformedDateParamsAreRejected is the one that matters most for the bot.
//
// These used to return 200 with an empty list, which is byte-identical to the
// response for a day that genuinely has no events. A bot that miscomputes its
// date parameter would post nothing, report success, and keep doing so
// indefinitely with nothing in any log to say why.
func TestMalformedDateParamsAreRejected(t *testing.T) {
	bad := []string{
		"month=abc&day=9", "month=13&day=1", "month=0&day=1",
		"day=32&month=8", "day=0&month=8", "day=abc&month=8",
		"month=8.0&day=9", "month=+8&day=9", "year=abc",
		"year=20260", "month=%208&day=9",
	}

	for _, q := range bad {
		t.Run(q, func(t *testing.T) {
			var body map[string]interface{}
			code := get(t, "/api/events?lang=ru&"+q, &body)
			if code != http.StatusBadRequest {
				t.Errorf("%s returned %d, want 400 — an unparseable date filter must "+
					"not be indistinguishable from a day with no events", q, code)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, "invalid") {
				t.Errorf("%s: unhelpful error %q", q, msg)
			}
		})
	}
}

// TestValidDateParamsStillWork is the control for the test above: rejecting
// everything would pass it. Single- and double-digit forms must both work,
// since callers build these from integers and rarely zero-pad.
func TestValidDateParamsStillWork(t *testing.T) {
	good := []struct {
		query string
		want  int // events expected, -1 to only require 200
	}{
		{"month=8&day=9", 1},   // Hal Finney's last post, in both fixtures
		{"month=08&day=09", 1}, // same day, zero-padded
		{"month=8", -1},
		{"day=9", -1},
		{"year=2013", -1},
		{"month=12&day=31", -1},
		{"month=1&day=1", -1},
	}

	for _, tc := range good {
		t.Run(tc.query, func(t *testing.T) {
			var body struct {
				Events []struct {
					Date string `json:"date"`
				} `json:"events"`
			}
			if code := get(t, "/api/events?lang=ru&"+tc.query, &body); code != http.StatusOK {
				t.Fatalf("%s returned %d, want 200", tc.query, code)
			}
			if tc.want >= 0 && len(body.Events) != tc.want {
				t.Errorf("%s returned %d events, want %d", tc.query, len(body.Events), tc.want)
			}
		})
	}
}

// TestPaginationParamsAreRejected extends to page and limit the rule
// TestMalformedDateParamsAreRejected established for the date filters: a
// parameter the service cannot honour is a 400, not a silent substitution.
//
// These three used to be defaulted without a word — `limit=abc`, `limit=0` and
// `limit=-5` all came back as page 1 of 20, with a 200 and a body that looks
// exactly like a correct answer to a different question.
//
// The upper bound is the other half. `limit=100000` served the entire artifact
// in one response: not a load problem at this corpus size, but it lets a caller
// page through everything by accident and never discover that the endpoint is
// paginated at all.
func TestPaginationParamsAreRejected(t *testing.T) {
	bad := []string{
		"page=abc", "page=0", "page=-1", "page=1.5", "page=+2", "page=99999999",
		"limit=abc", "limit=0", "limit=-5", "limit=1001", "limit=100000",
	}

	// Every list endpoint parses these through the same helper; if one is ever
	// wired up differently, it is this list that says so.
	paths := []string{
		"/api/events?lang=ru&",
		"/api/events/tags/bitcoin?lang=ru&",
		"/api/search?lang=ru&q=bitcoin&",
	}

	for _, p := range paths {
		for _, q := range bad {
			t.Run(p+q, func(t *testing.T) {
				var body map[string]interface{}
				if code := getAs(t, apiKey3, p+q, &body); code != http.StatusBadRequest {
					t.Errorf("%s%s returned %d, want 400", p, q, code)
				}
				msg, _ := body["error"].(string)
				if !strings.Contains(msg, "invalid") {
					t.Errorf("%s%s: unhelpful error %q", p, q, msg)
				}
			})
		}
	}
}

// TestPaginationParamsStillWork is the control: rejecting everything would
// satisfy the test above. The boundary values in particular must be accepted,
// since an off-by-one in the bound would only ever show up here.
func TestPaginationParamsStillWork(t *testing.T) {
	good := []string{"", "page=1", "limit=1", "limit=20", "limit=100", "limit=1000", "page=2&limit=1"}

	for _, q := range good {
		t.Run(q, func(t *testing.T) {
			var list eventList
			if code := getAs(t, apiKey3, "/api/events?lang=ru&"+q, &list); code != http.StatusOK {
				t.Errorf("%s returned %d, want 200", q, code)
			}
		})
	}
}

// TestListOrderBreaksTiesById pins the ordering contract from the outside: two
// events sharing a date come back newest-id first, and paging one at a time
// shows every event exactly once.
//
// Be clear about what this does and does not prove. It does not fail on the
// unfixed code — with the tiebreaker removed it still passes, because SQLite
// answers `ORDER BY date desc` from a backward scan of idx_events_date, which
// happens to yield descending rowid within a date. That is a property of the
// plan, not a guarantee: SQL promises no order among rows the ORDER BY cannot
// separate, and a new index or a different SQLite build can change the answer
// without a line of this service changing.
//
// So this is the contract, asserted where a client can see it, and it will
// catch such a change the day it happens. TestPaginatedSortsBreakTies is the
// half that catches the sort losing its tiebreaker in the first place.
func TestListOrderBreaksTiesById(t *testing.T) {
	var list eventList
	if code := getAs(t, apiKey3, "/api/events?lang=ru&limit=100", &list); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}

	var tied []int
	for _, e := range list.Events {
		if e.Date == "2008-11-01" {
			tied = append(tied, e.ID)
		}
	}
	if len(tied) != 2 {
		t.Fatalf("want 2 events on 2008-11-01, got %d — the fixture no longer "+
			"contains a tie and this test cannot fail", len(tied))
	}
	if tied[0] != 5 || tied[1] != 2 {
		t.Errorf("events sharing 2008-11-01 came back as %v, want [5 2]: ties must "+
			"break on id so that paging cannot repeat one event and drop another", tied)
	}

	// Paging one at a time must enumerate every event exactly once. This is the
	// property the ordering exists to provide; the assertion above is how it is
	// achieved.
	seen := map[int]int{}
	for page := 1; page <= list.Pagination.Total; page++ {
		var one eventList
		if code := getAs(t, apiKey3, fmt.Sprintf("/api/events?lang=ru&limit=1&page=%d", page), &one); code != http.StatusOK {
			t.Fatalf("page %d: want 200, got %d", page, code)
		}
		if len(one.Events) != 1 {
			t.Fatalf("page %d returned %d events, want 1", page, len(one.Events))
		}
		seen[one.Events[0].ID]++
	}
	for _, e := range list.Events {
		switch seen[e.ID] {
		case 1:
		case 0:
			t.Errorf("event %d never appeared while paging one at a time", e.ID)
		default:
			t.Errorf("event %d appeared %d times while paging one at a time", e.ID, seen[e.ID])
		}
	}
}

// TestRateLimitIsPerAPIKey proves two callers get two budgets.
//
// Every consumer of this service runs on the same box and reaches it over
// loopback, so keying the limiter on the client IP gave all of them one shared
// 100/min bucket — the Telegram bot, the site and anything else quietly
// throttling each other, with intermittent 429s as the only symptom.
//
// The measurement is a delta rather than an exhaustion: spending budget on one
// key must not move the other key's counter. Exhausting a 100/min bucket would
// leave the rest of the suite rate-limited for the following minute.
func TestRateLimitIsPerAPIKey(t *testing.T) {
	remaining := func(key string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, baseURL+"/api/events?limit=1", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("X-API-KEY", key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("key %q got %d, want 200", key, res.StatusCode)
		}
		v := res.Header.Get("X-RateLimit-Remaining")
		if v == "" {
			t.Skip("the limiter emits no X-RateLimit-Remaining; nothing to measure")
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("X-RateLimit-Remaining = %q: %v", v, err)
		}
		return n
	}

	const spentOnOther = 5

	before := remaining(apiKey)
	for i := 0; i < spentOnOther; i++ {
		remaining(apiKey2)
	}
	after := remaining(apiKey)

	// Only the two probe requests on apiKey itself should have been charged
	// to it, so its counter moves by exactly one between the two readings.
	spent := before - after
	if spent != 1 {
		t.Errorf("spending %d requests on a second API key moved the first key's "+
			"budget by %d (%d -> %d), want 1: the limiter is keying on something "+
			"both callers share — every consumer is on loopback, so that is one "+
			"bucket for all of them", spentOnOther, spent, before, after)
	}
}
