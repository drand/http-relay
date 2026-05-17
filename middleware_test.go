package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAddCommonHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := addCommonHeaders(next)

	r := httptest.NewRequest(http.MethodGet, "/v2/chains", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, version, w.Header().Get("Server"))
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestGetLogLevel(t *testing.T) {
	old := *verbose
	defer func() { *verbose = old }()

	*verbose = false
	require.Equal(t, slog.LevelInfo, getLogLevel())

	*verbose = true
	require.Equal(t, slog.LevelDebug, getLogLevel())
}
