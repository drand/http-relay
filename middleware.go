package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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

// drandHandler is setting all the routes and middleware we need for a drand relay
func drandHandler(client *grpc.Client) http.Handler {
	// setup the chi router
	r := chi.NewRouter()

	// putting the metric middleware first to get timing right
	r.Use(prometheusMiddleware)

	// setup the logger middleware
	logger := httplog.NewLogger("drand-http-relay", httplog.Options{
		JSON:            *jsonFlag,
		LogLevel:        getLogLevel(),
		Concise:         !(*verbose),
		ResponseHeaders: *verbose,
		RequestHeaders:  false,
		QuietDownRoutes: []string{
			"/",
			"/ping",
		},
		QuietDownPeriod: 1 * time.Second,
	})

	// this also setups Request ID and Panic Recoverer middleware behind the hood
	r.Use(httplog.RequestLogger(logger))

	// setup the ping endpoint for load balancers and uptime testing, without ACLs
	r.Use(middleware.Heartbeat("/ping"))

	if *verbose {
		// when running in verbose mode, we have a special Debug log telling us for each request whether it was matched
		// or not by Chi against a given route.
		r.Use(trackRoute)
	}

	SetupRoutes(r, client)

	// we want to print all routes served by our chi router
	allRoutes := make([]string, 0, 20)
	// displays all existing routes in the CLI upon starting or calling /
	walkFunc := func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		// we don't show the special error route for max uint64
		if strings.Contains(route, "18446744073709551615") {
			return nil
		}
		route = strings.Replace(route, "/*/", "/", -1)
		allRoutes = append(allRoutes, fmt.Sprintf("%s %s", method, route))
		return nil
	}
	if err := chi.Walk(r, walkFunc); err != nil {
		fmt.Printf("Logging err: %s\n", err.Error())
	}
	r.Get("/", DisplayRoutes(allRoutes))

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

// addCommonHeaders is setting the json and CORS headers for drand json outputs
func addCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", version)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}
