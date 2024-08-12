package main

import (
	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
)

func SetupRoutes(r *chi.Mux, client *grpc.Client) {
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
}
