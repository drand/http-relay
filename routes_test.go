package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDisplayRoutes_FiltersAndFallsBack(t *testing.T) {
	// prepare a stable set of routes
	allRoutes = []string{
		"GET /v1/info",
		"GET /v2/chains",
		"GET /v2/chains/{chainhash}/info",
	}

	t.Run("filters by prefix", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2", nil)
		rr := httptest.NewRecorder()

		DisplayRoutes(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}

		body := rr.Body.String()
		if !strings.Contains(body, "GET /v2/chains") {
			t.Fatalf("expected filtered routes to contain v2 route, got %q", body)
		}
		if strings.Contains(body, "GET /v1/info") {
			t.Fatalf("expected v1 route to be filtered out, got %q", body)
		}
	})

	t.Run("falls back to all routes when no prefix matches", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
		rr := httptest.NewRecorder()

		DisplayRoutes(rr, req)

		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected status %d, got %d", http.StatusNotFound, rr.Code)
		}

		body := rr.Body.String()
		for _, route := range allRoutes {
			if !strings.Contains(body, route) {
				t.Fatalf("expected fallback to include %q, got %q", route, body)
			}
		}
	})
}

func TestDisplayRoutes_HeadersAndOrder(t *testing.T) {
	allRoutes = []string{
		"GET /v1/info",
		"GET /v2/chains",
		"GET /v2/chains/{chainhash}/info",
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	DisplayRoutes(rr, req)

	// Content-Type and other headers
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("expected Content-Type text/plain; charset=utf-8, got %q", ct)
	}
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options: nosniff")
	}

	// V2 routes must appear after V1 (DisplayRoutes sort order)
	body := rr.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	v1Idx, v2Idx := -1, -1
	for i, line := range lines {
		if strings.HasPrefix(line, "GET /v1") {
			v1Idx = i
		}
		if strings.HasPrefix(line, "GET /v2") {
			if v2Idx == -1 {
				v2Idx = i
			}
		}
	}
	if v1Idx >= 0 && v2Idx >= 0 && v1Idx > v2Idx {
		t.Fatalf("expected v2 routes after v1: v1 at %d, v2 at %d; body:\n%s", v1Idx, v2Idx, body)
	}
}
