package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v2"
	"github.com/golang-jwt/jwt/v5"
)

var version = "drand-http-server-v2.0.1"

func main() {
	grpcURL := flag.String("grpc", "localhost:7001", "The URL and port to your drand node's grpc port, e.g. pl1-rpc.testnet.drand.sh:443")
	goVersion := flag.Bool("version", false, "Displays the current server version.")
	flag.Parse()
	if *goVersion {
		log.Fatal("drand http server version: ", version)
	}
	log.Println("Starting with", "grpc", *grpcURL)
	host, port, err := net.SplitHostPort(*grpcURL)
	if err != nil {
		log.Fatal("Unable to parse --grpc flag, please provide a valid one. Err: ", err)
	}

	log.Println("Starting ", version, " against GRPC node at ", host)

	// Logger
	logger := httplog.NewLogger("httplog-example", httplog.Options{
		// JSON:             true,
		LogLevel:       slog.LevelDebug,
		Concise:        false,
		RequestHeaders: true,
		// TimeFieldFormat: time.RFC850,
		QuietDownRoutes: []string{
			"/",
			"/ping",
		},
		QuietDownPeriod: 10 * time.Second,
		// SourceFieldName: "source",
	})

	r := chi.NewRouter()
	r.Use(httplog.RequestLogger(logger))
	r.Use(middleware.Heartbeat("/ping"))
	r.Use(middleware.Recoverer)
	r.Use(AddCommonHeaders)

	client, err := grpc.NewClient(net.JoinHostPort(host, port))
	if err != nil {
		log.Fatalf("Failed to create client: %s", err)
	}

	r.Get("/login", GetJWT)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(AddAuth)
		r.Get("/{chainid}/{round}", GetBeacon(client))
	})

	http.ListenAndServe(":8080", r)
}

func AddCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", version)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func AddAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.Split(r.Header.Get("Authorization"), "Bearer ")
		if len(authHeader) != 2 {
			log.Println("Received invalid request", authHeader)

			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		log.Println("Received request", authHeader)

		token, err := jwt.Parse(authHeader[1], func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		})

		if err != nil {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		if !token.Valid {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

var jwtSecret = []byte("our-secret")

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

	fmt.Println("JWT token is:", tokenString)
	json.NewEncoder(w).Encode(response)
}

func GetBeacon(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		roundStr := chi.URLParam(r, "round")
		_ = chi.URLParam(r, "chainId")

		round, err := strconv.ParseUint(roundStr, 10, 64)
		if err != nil {
			http.Error(w, "Failed to parse round. Err: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Assuming we have a method in our gRPC client to get a beacon by round
		beacon, err := c.GetBeacon("", round)
		if err != nil {
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}
		json, err := json.Marshal(NewHexBeacon(beacon))
		if err != nil {
			http.Error(w, "Failed to Encode beacon in hex", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}
