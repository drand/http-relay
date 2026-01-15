package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestReadRound(t *testing.T) {
	testCases := []struct {
		name        string
		round       string
		expectError bool
		expectedMsg string
		expectedVal uint64
		description string
	}{
		{
			name:        "empty round parameter",
			round:       "",
			expectError: true,
			expectedMsg: "round parameter is required",
			description: "empty round should return error",
		},
		{
			name:        "invalid format - non-numeric",
			round:       "abc",
			expectError: true,
			expectedMsg: "invalid round number",
			description: "non-numeric round should return error",
		},
		{
			name:        "invalid format - negative",
			round:       "-1",
			expectError: true,
			expectedMsg: "invalid round number",
			description: "negative round should return error",
		},
		{
			name:        "invalid format - decimal",
			round:       "1.5",
			expectError: true,
			expectedMsg: "invalid round number",
			description: "decimal round should return error",
		},
		{
			name:        "extremely large number - parse error",
			round:       "999999999999999999999999999999999999999999999999999999999999999999999999",
			expectError: true,
			expectedMsg: "invalid round number",
			description: "extremely large round (parse error) should return error",
		},
		{
			name:        "large but parseable number exceeding reasonable max",
			round:       "1152921504606846977", // 2^60 + 1
			expectError: true,
			expectedMsg: "exceeds maximum allowed value",
			description: "round exceeding reasonable maximum should return error",
		},
		{
			name:        "valid round number",
			round:       "1",
			expectError: false,
			expectedVal: 1,
			description: "valid round should parse successfully",
		},
		{
			name:        "valid large round number",
			round:       "1152921504606846976", // 2^60 (max allowed)
			expectError: false,
			expectedVal: 1152921504606846976,
			description: "valid large round at maximum should parse successfully",
		},
		{
			name:        "zero round",
			round:       "0",
			expectError: false,
			expectedVal: 0,
			description: "zero round should parse successfully",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a chi router and request
			r := chi.NewRouter()
			r.Get("/test/{round}", func(w http.ResponseWriter, r *http.Request) {
				round, err := readRound(r)
				if tc.expectError {
					require.Error(t, err, tc.description)
					if tc.expectedMsg != "" {
						require.Contains(t, err.Error(), tc.expectedMsg, tc.description)
					}
				} else {
					require.NoError(t, err, tc.description)
					require.Equal(t, tc.expectedVal, round, tc.description)
				}
			})

			req := httptest.NewRequest("GET", "/test/"+tc.round, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		})
	}
}

func TestReadRoundDirect(t *testing.T) {
	// Test empty round parameter
	r := chi.NewRouter()
	r.Get("/test/{round}", func(w http.ResponseWriter, r *http.Request) {
		round, err := readRound(r)
		require.Error(t, err)
		require.Contains(t, err.Error(), "round parameter is required")
		require.Equal(t, uint64(0), round)
	})
	req := httptest.NewRequest("GET", "/test/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Test valid round
	r = chi.NewRouter()
	r.Get("/test/{round}", func(w http.ResponseWriter, r *http.Request) {
		round, err := readRound(r)
		require.NoError(t, err)
		require.Equal(t, uint64(123), round)
	})
	req = httptest.NewRequest("GET", "/test/123", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Test invalid round
	r = chi.NewRouter()
	r.Get("/test/{round}", func(w http.ResponseWriter, r *http.Request) {
		round, err := readRound(r)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid round number")
		require.Equal(t, uint64(0), round)
	})
	req = httptest.NewRequest("GET", "/test/abc", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	
	// Test round exceeding maximum
	maxRound := strconv.FormatUint(uint64(1<<60)+1, 10)
	r = chi.NewRouter()
	r.Get("/test/{round}", func(w http.ResponseWriter, r *http.Request) {
		round, err := readRound(r)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds maximum allowed value")
		require.Equal(t, uint64(0), round)
	})
	req = httptest.NewRequest("GET", "/test/"+maxRound, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
}
