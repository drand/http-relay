package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

func AddAuth(next http.Handler) http.Handler {
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

func GetJWT(w http.ResponseWriter, r *http.Request) {
	// Create a new token object
	token := jwt.New(jwt.SigningMethodHS256)

	// Create a JWT and send it as response
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		http.Error(w, "Error while signing the token", http.StatusInternalServerError)
		return
	}

	response := map[string]string{
		"token": tokenString,
	}

	slog.Info("Emitted a JWT token", "token", tokenString)
	json.NewEncoder(w).Encode(response)
}
