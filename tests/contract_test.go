package tests

import (
	"encoding/json"
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
