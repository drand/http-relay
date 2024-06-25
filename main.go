package main

import (
	"context"
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
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version     = "drand-http-server-v2.0.1"
	metricFlag  = flag.String("metrics", "localhost:9999", "The flag to set the interface for metrics. Defaults to localhost:9999")
	httpBind    = flag.String("bind", "localhost:8080", "The address to bind the http server to")
	grpcURL     = flag.String("grpc-connect", "localhost:4444", "The URL and port to your drand node's grpc port, e.g. pl1-rpc.testnet.drand.sh:443 you can add fallback nodes by separating them with a comma: pl1-rpc.testnet.drand.sh:443,pl2-rpc.testnet.drand.sh:443")
	goVersion   = flag.Bool("version", false, "Displays the current server version.")
	requireAuth = flag.Bool("enable-auth", false, "Forces JWT authentication on V2 API using the JWT secret from the AUTH_TOKEN env variable.")
	verbose     = flag.Bool("verbose", false, "Prints as many logs as possible.")
	jsonFlag    = flag.Bool("json", false, "Prints logs in JSON format.")
	frontrun    = flag.Int64("frontrun", 0, "When waiting for the next round, start the query this amount of ms earlier to counteract network latency.")
	_           = flag.Bool("insecure", false, "deprecated flag")
	_           = flag.String("hash-list", "", "deprecated flag")
)

func init() {
	flag.Parse()
	slog.SetLogLoggerLevel(getLogLevel())
	if *frontrun > 0 {
		FrontrunTiming = time.Duration(*frontrun) * time.Millisecond
	}
}

func main() {
	if *goVersion {
		log.Fatal("drand http server version: ", version)
	}

	nodesAddr := strings.Split(*grpcURL, ",")
	for _, nodeAdd := range nodesAddr {
		_, _, err := net.SplitHostPort(nodeAdd)
		if err != nil {
			log.Fatalf("Unable to parse --grpc flag correctly, please provide valid node URLs. On %q, got err: %v", nodeAdd, err)
		}
	}

	client, err := grpc.NewClient("fallback:///"+*grpcURL, slog.Default())
	if err != nil {
		log.Fatal("Failed to create client", "address", nodesAddr, "error", err)
	}
	defer client.Close()

	go serveMetrics()

	slog.Info("Starting http relay", "version", version, "client", client)

	// The HTTP Server
	server := &http.Server{Addr: *httpBind, Handler: setup(client)}

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to exit gracefully
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		defer serverStopCtx()
		s := <-sig

		slog.Info("Caught interrupt, shutting down...", "signal", s.String())

		// Shutdown signal with grace period of 30 seconds
		shutdownCtx, _ := context.WithTimeout(serverCtx, 30*time.Second)
		go func() {
			<-shutdownCtx.Done()
			if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
				slog.Error("graceful shutdown timed out.. forcing exit")
				return
			}
		}()

		// Trigger graceful shutdown
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("server Shutdown error", "err", err)
			return
		}
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

func prometheusMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn := promhttp.InstrumentHandlerCounter(
			HTTPCallCounter,
			promhttp.InstrumentHandlerDuration(
				HTTPLatency,
				promhttp.InstrumentHandlerInFlight(
					HTTPInFlight,
					next)))
		// We could also instrument:
		// 	- time to write headers, but since we have common headers, these aren't too useful
		//  - request size, but these are supposedly fixed size and are in the logs
		fn.ServeHTTP(w, r)
	})
}

func setup(client *grpc.Client) http.Handler {
	// setup chi router
	r := chi.NewRouter()

	// putting the metric middleware first to get timing right
	r.Use(prometheusMiddleware)

	// setup logger middleware
	logger := httplog.NewLogger("drand-http-relay", httplog.Options{
		JSON:            *jsonFlag,
		LogLevel:        getLogLevel(),
		Concise:         !(*verbose),
		ResponseHeaders: *verbose,
		// TimeFieldFormat: time.RFC850,
		// RequestHeaders:  true, // not supported by our logger
		// QuietDownRoutes: []string{ // not supported by our logger
		//	 "/",
		//	 "/ping",
		// },
		// QuietDownPeriod: 1 * time.Second, // not supported by our logger
	})
	// this also setups Request ID and Panic recoverer middleware behind the hood
	r.Use(httplog.RequestLogger(logger))

	// setup ping endpoint for load balancers and uptime testing, without ACLs
	r.Use(middleware.Heartbeat("/ping"))

	if *verbose {
		// when running in verbose mode, we have a special Debug log telling us for each request whether it was matched
		// or not by Chi against a given route.
		r.Use(trackRoute)
	}

	// For now, rate-limiting is left to the nginx reverse proxy...
	//// Only 5 requests will be processed at a time.
	//r.Use(middleware.Throttle(5))

	r.Get("/public/18446744073709551615", sendMaxInt())

	// v2 with ACL protected routes with shared grpc client
	r.Group(func(r chi.Router) {
		// JWT authentication, tokens to be issued using the jwtissuer binary
		if *requireAuth {
			r.Use(AddAuth)
		}
		r.Use(apiVersionCtx("v2"))
		r.Route("/v2", func(r chi.Router) {
			// use our common headers for the following routes
			r.Use(addCommonHeaders)
			r.Get("/chains", GetChains(client))

			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/info", GetInfoV2(client))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/health", GetHealth(client))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/{round:\\d+}", GetBeacon(client, true))
			r.Get("/{chainhash:[0-9A-Fa-f]{64}}/latest", GetLatest(client))

			r.Get("/{beaconID}/info", GetInfoV2(client))
			r.Get("/{beaconID}/health", GetHealth(client))
			r.Get("/{beaconID}/{round:\\d+}", GetBeacon(client, true))
			r.Get("/{beaconID}/latest", GetLatest(client))

		})
	})

	// v1 API
	r.Group(func(r chi.Router) {
		// use our common headers for the following routes
		r.Use(addCommonHeaders)

		r.Use(apiVersionCtx("v1"))

		r.Get("/chains", GetChains(client))
		r.Get("/info", GetInfoV1(client))
		r.Get("/health", GetHealth(client))

		r.Get("/public/latest", GetLatest(client))
		r.Get("/public/{round:\\d+}", GetBeacon(client, false))

		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/latest", GetLatest(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/{round:\\d+}", GetBeacon(client, false))
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
		slog.Debug("request matched", "route", routePattern)
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

func getLogLevel() slog.Level {
	if *verbose {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}
