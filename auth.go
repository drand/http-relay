package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// AddAuth relies on the DRAND_AUTH_KEY env variable to setup JWT authentication on the v2 API endpoints.
func AddAuth(next http.Handler) http.Handler {
	token, provided := os.LookupEnv("DRAND_AUTH_KEY")
	if !provided || len(token) < 256 {
		log.Fatal("DRAND_AUTH_KEY not set to a 128 byte hex-encoded secret, disabling authenticated API")
	}

	jwtSecret, err := hex.DecodeString(token)
	if err != nil {
		slog.Error("unable to parse DRAND_AUTH_KEY as valid hex, disabling authenticated API")
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.Split(r.Header.Get("Authorization"), "Bearer ")
		if len(authHeader) != 2 {
			slog.Error("Received invalid request, JWT not recognized", "Authorization", authHeader)

			http.Error(w, "Missing JWT", http.StatusUnauthorized)
			return
		}

		token, err := jwt.Parse(authHeader[1], func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		}, jwt.WithValidMethods([]string{"HS256,HS384"}))
		if err != nil {
			slog.Error("Unable to parse JWT!", "err", err)
			http.Error(w, "Invalid JWT", http.StatusUnauthorized)
			return
		}

		if !token.Valid {
			slog.Error("Received an invalid JWT!", "from", r.RemoteAddr, "uri", r.RequestURI)

			http.Error(w, "Invalid JWT", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
