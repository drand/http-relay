package main

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

func AddAuth(next http.Handler) http.Handler {
	var jwtSecret []byte

	// TODO: consider migrating to AWS secret manager instead of using env variables
	token, provided := os.LookupEnv("AUTH_TOKEN")
	if !provided || len(token) < 256 {
		slog.Warn("AUTH_TOKEN not set to a 128 byte hex-encoded secret, disabling authenticated API")
		return next
	} else {
		var err error
		jwtSecret, err = hex.DecodeString(token)
		if err != nil {
			slog.Error("unable to parse AUTH_TOKEN as valid hex, disabling authenticated API")
			return next
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.Split(r.Header.Get("Authorization"), "Bearer ")
		if len(authHeader) != 2 {
			slog.Error("Received invalid request, JWT not recognized", "Authorization", authHeader)

			http.Error(w, "Invalid JWT token", http.StatusUnauthorized)
			return
		}

		slog.Debug("Received JWT authenticated request", "headers", authHeader)

		token, err := jwt.Parse(authHeader[1], func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		}, jwt.WithValidMethods([]string{"HS256,HS384"}))
		if err != nil {
			slog.Error("Unable to parse JWT token!", "err", err)
			http.Error(w, "Invalid JWT token", http.StatusUnauthorized)
			return
		}

		if !token.Valid {
			slog.Error("Received invalid token!", "from", r.RemoteAddr, "uri", r.RequestURI)

			http.Error(w, "Invalid JWT token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

//// GetJWT is an endpoint returning a valid JWT, be careful not to expose it where it shouldn't be exposed!
//func GetJWT(w http.ResponseWriter, r *http.Request) {
//	// Create a new token object
//	token := jwt.New(jwt.SigningMethodHS256)
//
//	// Create a JWT and send it as response
//	tokenString, err := token.SignedString(jwtSecret)
//	if err != nil {
//		http.Error(w, "Error while signing the token", http.StatusInternalServerError)
//		return
//	}
//
//	response := map[string]string{
//		"token": tokenString,
//	}
//
//	slog.Info("Emitted a JWT token", "token", tokenString)
//	json.NewEncoder(w).Encode(response)
//}
