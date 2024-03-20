package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v2"
)

var (
	jwtSecret   []byte
	version     = "drand-http-server-v2.0.1"
	grpcURL     = flag.String("grpc", "localhost:7001", "The URL and port to your drand node's grpc port, e.g. pl1-rpc.testnet.drand.sh:443")
	goVersion   = flag.Bool("version", false, "Displays the current server version.")
	requireAuth = flag.Bool("enable-auth", false, "Forces JWT authentication on V2 API using the JWT secret from .")
)

func init() {
	flag.Parse()
	// TODO: consider migrating to AWS secret manager
	token, provided := os.LookupEnv("AUTH_TOKEN")
	if !provided || len(token) < 256 {
		slog.Warn("AUTH_TOKEN not set to a 128 byte hex-encoded secret, disabling authenticated API")
		*requireAuth = false
	} else {
		var err error
		jwtSecret, err = hex.DecodeString(token)
		if err != nil {
			slog.Error("unable to parse AUTH_TOKEN as valid hex, disabling authenticated API")
			*requireAuth = false
		}
	}
}

func main() {
	if *goVersion {
		log.Fatal("drand http server version: ", version)
	}

	host, port, err := net.SplitHostPort(*grpcURL)
	if err != nil {
		log.Fatal("Unable to parse --grpc flag, please provide a valid one. Err: ", err)
	}

	slog.Info("Starting http relay", "version", version, "host", host)
	client, err := grpc.NewClient(net.JoinHostPort(host, port), slog.Default())
	if err != nil {
		slog.Error("Failed to create client", "error", err)
		return
	}
	defer client.Close()

	// The HTTP Server
	server := &http.Server{Addr: "0.0.0.0:8080", Handler: service(client)}

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to exit gracefully
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig

		// Shutdown signal with grace period of 30 seconds
		shutdownCtx, cancel := context.WithTimeout(serverCtx, 30*time.Second)

		go func() {
			<-shutdownCtx.Done()
			if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
				slog.Error("graceful shutdown timed out.. forcing exit")
				return
			}
			// make sure to cancel early if we can
			cancel()
		}()

		// Trigger graceful shutdown
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			slog.Error("server Shutdown error", "err", err)
			return
		}
		serverStopCtx()
	}()

	// Run the server
	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		return
	}

	// Wait for server context to be stopped
	<-serverCtx.Done()
	slog.Info("drand http server stopped")
}

func service(client *grpc.Client) http.Handler {
	// setup chi router
	r := chi.NewRouter()

	// setup logger middleware
	logger := httplog.NewLogger("http-relay", httplog.Options{
		//JSON:           true,
		LogLevel:        slog.LevelInfo,
		Concise:         true,
		RequestHeaders:  true,
		ResponseHeaders: true,
		// TimeFieldFormat: time.RFC850,
		QuietDownRoutes: []string{
			"/",
			"/ping",
		},
		QuietDownPeriod: 1 * time.Second,
		// SourceFieldName: "source",
	})
	r.Use(httplog.RequestLogger(logger))

	// setup ping endpoint for load balancers and uptime testing, without ACLs
	r.Use(middleware.Heartbeat("/ping"))

	// setup panic recoverer to report panics as 500 errors instead of crashing
	r.Use(middleware.Recoverer)
	r.Use(trackRoute)

	// login route is a debug route to get a JWT, will need better handling in the future
	if *requireAuth {
		r.Get("/login", GetJWT)
	}

	// v2 with ACL protected routes with shared grpc client
	r.Group(func(r chi.Router) {
		// JWT authentication
		if *requireAuth {
			r.Use(AddAuth)
		}
		r.Use(apiVersionCtx("v2"))
		r.Route("/v2", func(r chi.Router) {
			// setup our common headers
			r.Use(addCommonHeaders)
			r.Get("/chains", GetChains(client))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/{round:\\d+}", GetBeacon(client))

			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/latest", GetLatest(client))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/info", GetInfoV2(client))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/health", GetHealth(client))

			r.Get("/{beaconID}/latest", GetLatest(client))
			r.Get("/{beaconID}/{round:\\d+}", GetBeacon(client))
			r.Get("/{beaconID}/info", GetInfoV2(client))
			r.Get("/{beaconID}/health", GetHealth(client))
		})
	})

	// v1 API
	r.Group(func(r chi.Router) {
		// setup our common headers
		r.Use(addCommonHeaders)
		// Only 5 requests will be processed at a time (supposedly the caching should handle the others).
		r.Use(middleware.Throttle(5))
		r.Use(apiVersionCtx("v1"))
		r.Get("/chains", GetChains(client))
		r.Get("/info", GetInfoV1(client))
		r.Get("/public/latest", GetLatest(client))
		r.Get("/public/{round:\\d+}", GetBeacon(client))
		r.Get("/health", GetHealth(client))

		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/latest", GetLatest(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/{round:\\d+}", GetBeacon(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/info", GetInfoV1(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/health", GetHealth(client))
	})

	{
		// we want to print all routes served by our chi router
		allRoutes := &strings.Builder{}
		// displays all existing routes in the CLI upon starting or calling /
		walkFunc := func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
			route = strings.Replace(route, "/*/", "/", -1)
			fmt.Fprintf(allRoutes, "%s %s\n", method, route)
			return nil
		}
		if err := chi.Walk(r, walkFunc); err != nil {
			fmt.Printf("Logging err: %s\n", err.Error())
		}
		r.Get("/", DisplayRoutes([]byte(allRoutes.String())))
	}

	// we explicitly don't serve favicon
	r.Get("/favicon.ico", http.NotFound)

	return r
}

func trackRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		rctx := chi.RouteContext(r.Context())
		routePattern := strings.Join(rctx.RoutePatterns, "")
		slog.Debug("request received", "route", routePattern)
	})
}

func addCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", version)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

func apiVersionCtx(version string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), "api.version", version))
			next.ServeHTTP(w, r)
		})
	}
}
