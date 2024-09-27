package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
)

// allRoutes is populated in SetupRoutes after all routes have been setup
// and used in DisplayRoutes to display available routes
var allRoutes []string

func DisplayRoutes(w http.ResponseWriter, r *http.Request) {
	// Extract the prefix query parameter.
	prefixes := strings.Trim(r.URL.Path, "/")
	// Filter the routes that match the prefix, if any. We copy allRoutes to avoid changing its content.
	filteredRoutes := make([]string, len(allRoutes))
	copy(filteredRoutes, allRoutes)
	for _, prefix := range strings.Split(prefixes, "/") {
		n := 0
		for _, route := range filteredRoutes {
			if strings.Contains(route, prefix) {
				filteredRoutes[n] = route
				n++
			}
		}
		// we only update our list if some items matched
		if n > 0 {
			filteredRoutes = filteredRoutes[:n]
		}
	}

	// in case we didn't have any matches, let's show all routes
	if len(filteredRoutes) == 0 {
		filteredRoutes = allRoutes
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")

	slices.SortFunc(filteredRoutes, func(a, b string) int {
		// cmp(a, b) should return a negative number when a < b, a positive number when
		// a > b and zero when a == b.
		if strings.HasPrefix(a, "GET /v2") && !strings.HasPrefix(b, "GET /v2") {
			return 1
		} else if !strings.HasPrefix(a, "GET /v2") && strings.HasPrefix(b, "GET /v2") {
			return -1
		}
		return strings.Compare(a, b)
	})

	// We still report it as a 404
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(strings.Join(filteredRoutes, "\n")))
}

func SetupRoutes(r *chi.Mux, client *grpc.Client) {
	// Catch-all route for any other GET request, we display routes instead
	// we need to declare that before setup to avoid the r.Group to match first
	r.NotFound(DisplayRoutes)

	r.Get("/public/18446744073709551615", sendMaxInt())

	// v2 routes with optional ACL using JWT
	r.Group(func(r chi.Router) {
		// JWT authentication, tokens to be issued using the jwtissuer binary
		if *requireAuth {
			r.Use(AddAuth)
		}
		r.Route("/v2", func(r chi.Router) {
			// use our common headers for the following routes
			r.Use(addCommonHeaders)
			r.Get("/chains", GetChains(client))

			r.Get("/chains/{chainhash:[0-9A-Fa-f]{64}}/info", GetInfoV2(client))
			r.Get("/chains/{chainhash:[0-9A-Fa-f]{64}}/health", GetHealth(client))
			r.Get("/chains/{chainhash:[0-9A-Fa-f]{64}}/rounds/{round:\\d+}", GetBeacon(client, true))
			r.Get("/chains/{chainhash:[0-9A-Fa-f]{64}}/rounds/latest", GetLatest(client, true))
			r.Get("/chains/{chainhash:[0-9A-Fa-f]{64}}/rounds/next", GetNext(client))

			r.Get("/beacons", GetBeaconIds(client))
			r.Get("/beacons/{beaconID}/info", GetInfoV2(client))
			r.Get("/beacons/{beaconID}/health", GetHealth(client))
			r.Get("/beacons/{beaconID}/rounds/{round:\\d+}", GetBeacon(client, true))
			r.Get("/beacons/{beaconID}/rounds/latest", GetLatest(client, true))
			r.Get("/beacons/{beaconID}/rounds/next", GetNext(client))
		})
	})

	// v1 API CANNOT BE CHANGED until deprecation
	r.Group(func(r chi.Router) {
		// use our common headers for the following routes
		r.Use(addCommonHeaders)

		r.Get("/chains", GetChains(client))

		r.Get("/info", GetInfoV1(client))
		r.Get("/health", GetHealth(client))
		r.Get("/public/{round:\\d+}", GetBeacon(client, false))
		r.Get("/public/latest", GetLatest(client, false))

		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/info", GetInfoV1(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/health", GetHealth(client))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/{round:\\d+}", GetBeacon(client, false))
		r.Get("/{chainhash:[0-9A-Fa-f]{64}}/public/latest", GetLatest(client, false))
	})

	// we want to populate all the routes served by our Chi router to display them in DisplayRoutes
	allRoutes = make([]string, 0, 22)
	// need to populate the all routes slice to display all existing routes
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
}
