package tests

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestPublicEventsV2Permalinks(t *testing.T) {
	for _, lang := range []string{"ru", "en"} {
		t.Run(lang, func(t *testing.T) {
			res, body := publicGet(t, baseURL, "/public/v2/events?lang="+lang, nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("unauthenticated v2: want 200, got %d", res.StatusCode)
			}
			var doc struct {
				publicEventsDoc
				Events []struct {
					publicEvent
					URLPath string `json:"url_path"`
				} `json:"events"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Schema != "bitcoin-calendar.public-events.v2" || doc.Language != lang {
				t.Fatalf("unexpected envelope: %s %s", doc.Schema, doc.Language)
			}
			v1 := publicEvents(t, baseURL, publicEventsPath+"?lang="+lang)
			if !reflect.DeepEqual(doc.Database, v1.Database) || len(doc.Events) != len(v1.Events) {
				t.Fatal("v2 must retain the complete v1 document")
			}
			paths := map[int]string{}
			for _, row := range fixtureRows(lang) {
				paths[row.ID] = row.URLPath
			}
			for i, event := range doc.Events {
				if event.URLPath != paths[event.ID] {
					t.Errorf("event %d: stored path lost: %q", event.ID, event.URLPath)
				}
				if !reflect.DeepEqual(event.publicEvent, v1.Events[i]) {
					t.Errorf("event %d: v1 fields or order changed", event.ID)
				}
			}
			v1Res, v1Body := publicGet(t, baseURL, publicEventsPath+"?lang="+lang, nil)
			var raw struct {
				Events []map[string]json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(v1Body, &raw); err != nil {
				t.Fatal(err)
			}
			for _, event := range raw.Events {
				if _, exists := event["url_path"]; exists {
					t.Fatal("v1 contract must stay unchanged")
				}
			}
			etag := res.Header.Get("ETag")
			if etag == "" || etag == v1Res.Header.Get("ETag") {
				t.Fatal("v2 needs a distinct validator")
			}
			crossVersion, _ := publicGet(t, baseURL, "/public/v2/events?lang="+lang, map[string]string{"If-None-Match": v1Res.Header.Get("ETag")})
			if crossVersion.StatusCode != http.StatusOK {
				t.Fatal("a v1 validator must not suppress the v2 document")
			}
			cached, cachedBody := publicGet(t, baseURL, "/public/v2/events?lang="+lang, map[string]string{"If-None-Match": etag})
			if cached.StatusCode != http.StatusNotModified || len(cachedBody) != 0 {
				t.Fatal("v2 revalidation must return an empty 304")
			}
		})
	}
}
