package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestAddAuth_AllowsHS256AndHS384(t *testing.T) {
	// 128-byte key hex encoded (256 chars)
	secretHex := strings.Repeat("a", 256)
	t.Setenv("DRAND_AUTH_KEY", secretHex)

	secret, err := hex.DecodeString(secretHex)
	require.NoError(t, err)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := AddAuth(next)

	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
	}{
		{name: "hs256", method: jwt.SigningMethodHS256},
		{name: "hs384", method: jwt.SigningMethodHS384},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := jwt.New(tc.method)
			signed, err := token.SignedString(secret)
			require.NoError(t, err)

			r := httptest.NewRequest(http.MethodGet, "/v2/chains", nil)
			r.Header.Set("Authorization", "Bearer "+signed)
			w := httptest.NewRecorder()

			protected.ServeHTTP(w, r)
			require.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestAddAuth_RejectsHS512(t *testing.T) {
	// Ensure the env var is present to avoid log.Fatal in AddAuth initialization.
	secretHex := strings.Repeat("b", 256)
	t.Setenv("DRAND_AUTH_KEY", secretHex)

	secret, err := hex.DecodeString(secretHex)
	require.NoError(t, err)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := AddAuth(next)

	token := jwt.New(jwt.SigningMethodHS512)
	signed, err := token.SignedString(secret)
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "/v2/chains", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()

	protected.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAddAuth_MissingHeader(t *testing.T) {
	secretHex := strings.Repeat("c", 256)
	t.Setenv("DRAND_AUTH_KEY", secretHex)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := AddAuth(next)

	r := httptest.NewRequest(http.MethodGet, "/v2/chains", nil)
	w := httptest.NewRecorder()

	protected.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, "Missing JWT\n", w.Body.String())
}

func TestAddAuth_InvalidToken(t *testing.T) {
	secretHex := strings.Repeat("d", 256)
	t.Setenv("DRAND_AUTH_KEY", secretHex)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := AddAuth(next)

	r := httptest.NewRequest(http.MethodGet, "/v2/chains", nil)
	r.Header.Set("Authorization", "Bearer not-a-valid-jwt")
	w := httptest.NewRecorder()

	protected.ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, "Invalid JWT\n", w.Body.String())
}


