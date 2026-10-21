package tests

import (
	"io"
	"net/http"
	"strconv"
	"testing"
)

func TestAuthenticatedRateLimitAllows500Requests(t *testing.T) {
	service, err := startService(t, artifactDir)
	if err != nil {
		t.Fatalf("starting isolated service: %v", err)
	}
	for requestNumber := 1; requestNumber <= 501; requestNumber++ {
		endpoint := "/api/events?limit=1"
		if requestNumber%2 == 0 {
			endpoint = "/API/events?limit=1"
		}
		request, err := http.NewRequest(http.MethodGet, service.base+endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-API-KEY", apiKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if requestNumber <= 500 && response.Header.Get("X-RateLimit-Limit") != "500" {
			t.Fatalf("authenticated limit = %q, want 500", response.Header.Get("X-RateLimit-Limit"))
		}
		wantedStatus := http.StatusOK
		if requestNumber == 501 {
			wantedStatus = http.StatusTooManyRequests
			retryAfter, err := strconv.Atoi(response.Header.Get("Retry-After"))
			if err != nil || retryAfter < 1 || retryAfter > 60 {
				t.Fatalf("exhausted budget: invalid Retry-After = %q", response.Header.Get("Retry-After"))
			}
		}
		if response.StatusCode != wantedStatus {
			t.Fatalf("request %d: status = %d, want %d", requestNumber, response.StatusCode, wantedStatus)
		}
		if requestNumber <= 500 && response.Header.Get("X-RateLimit-Remaining") != strconv.Itoa(500-requestNumber) {
			t.Fatalf("request %d: remaining = %q", requestNumber, response.Header.Get("X-RateLimit-Remaining"))
		}
	}
}

func TestPublicAndUnauthenticatedRateLimitsRemain100(t *testing.T) {
	for _, endpoint := range []string{"/health", "/public/v1/events", "/public/v2/events", "/api/events"} {
		request, err := http.NewRequest(http.MethodGet, baseURL+endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if endpoint != "/api/events" {
			request.Header.Set("X-API-KEY", "invented-public-key")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.Header.Get("X-RateLimit-Limit") != "100" {
			t.Errorf("%s: limit = %q, want 100", endpoint, response.Header.Get("X-RateLimit-Limit"))
		}
		wantedStatus := http.StatusOK
		if endpoint == "/api/events" {
			wantedStatus = http.StatusUnauthorized
		}
		if response.StatusCode != wantedStatus {
			t.Errorf("%s: status = %d, want %d", endpoint, response.StatusCode, wantedStatus)
		}
	}
}
