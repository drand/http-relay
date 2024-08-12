package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/drand/drand/v2/common"
	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
)

var FrontrunTiming time.Duration

func DisplayRoutes(allRoutes []string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		slices.SortFunc(allRoutes, func(a, b string) int {
			// cmp(a, b) should return a negative number when a < b, a positive number when
			// a > b and zero when a == b.
			if strings.HasPrefix(a, "GET /v2") && !strings.HasPrefix(b, "GET /v2") {
				return 1
			} else if strings.HasPrefix(b, "GET /v2") && !strings.HasPrefix(a, "GET /v2") {
				return -1
			}
			return strings.Compare(a, b)
		})
		w.Write([]byte(strings.Join(allRoutes, "\n")))
	}
}

func GetBeacon(c *grpc.Client, isV2 bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}

		roundStr := chi.URLParam(r, "round")
		round, err := strconv.ParseUint(roundStr, 10, 64)
		if err != nil {
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
			http.Error(w, "Failed to parse round. Err: "+err.Error(), http.StatusBadRequest)
			return
		}

		info, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			slog.Error("[GetBeacon] error retrieving chain info from primary client", "error", err)
			// we will skip cache-age setting, something is wrong
			w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
			if errors.Is(err, context.Canceled) {
				http.Error(w, "timeout", http.StatusGatewayTimeout)
			} else if strings.Contains(err.Error(), "unknown chain hash") {
				http.Error(w, "unknown chain hash", http.StatusBadRequest)
			} else {
				http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			}
			return
		}

		nextTime, nextRound := info.ExpectedNext()
		if round >= nextRound+1 { // never happens when fetching latest because round == 0
			w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
			slog.Error("[GetBeacon] Future beacon was requested, unexpected", "requested", round, "expected", nextRound, "from", r.RemoteAddr)
			// I know, 425 is meant to indicate a replay attack risk, but hey, it's the perfect error name!
			http.Error(w, "Requested future beacon", http.StatusTooEarly)
			return
		} else if round == nextRound {
			// we wait until the round is supposed to be emitted, minus frontrun to account for network latency anyway
			time.Sleep(time.Duration(nextTime-time.Now().Unix())*time.Second - FrontrunTiming)
		}

		beacon, err := c.GetBeacon(r.Context(), m, round)
		if err != nil {
			if err != nil {
				slog.Error("all clients are unable to provide beacons", "error", err)
				w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
				http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
				return
			}
		}

		// TODO: should we rather use the api.version key from the request context set in apiVersionCtx?
		// the current way of doing it probably allows the compiler to inline the right path tho...
		if isV2 {
			// we make sure that the V2 api aren't marshaling randommness
			beacon.UnsetRandomness()
		} else {
			// we need to set the randomness since the nodes are not supposed to send it over the wire anymore
			beacon.SetRandomness()
		}

		json, err := json.Marshal(beacon)
		if err != nil {
			w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
			http.Error(w, "Failed to Encode beacon in hex", http.StatusInternalServerError)
			return
		}

		if round != 0 {
			// i.e. we're not fetching latest, we can store these beacons for a long time
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		} else {
			cacheTime := nextTime - time.Now().Unix()
			if cacheTime < 0 {
				cacheTime = 0
			}
			// we're fetching latest we need to stop caching in time for the next round
			w.Header().Set("Cache-Control",
				fmt.Sprintf("public, must-revalidate, max-age=%d", cacheTime))
			slog.Debug("[GetBeacon] StatusOK", "cachetime", cacheTime)
		}

		w.WriteHeader(http.StatusOK)
		w.Write(json)
	}
}

func GetChains(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		chains, err := c.GetChains(r.Context())
		if err != nil {
			if err != nil {
				slog.Error("failed to get chains from all clients", "error", err)
				http.Error(w, "Failed to get chains", http.StatusInternalServerError)
				return
			}
		}

		json, err := json.Marshal(chains)
		if err != nil {
			slog.Error("failed to encode chain in json", "error", err)
			http.Error(w, "Failed to encode chains", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetHealth(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// we never cache health requests (rate-limiting should prevent DoS at the proxy level)
		w.Header().Set("Cache-Control", "no-cache")

		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetHealth] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get health", http.StatusInternalServerError)
			return
		}

		latest, err := c.GetBeacon(r.Context(), m, 0)
		if err != nil {
			slog.Error("[GetHealth] failed to get latest beacon", "error", err)
			http.Error(w, "Failed to get latest beacon for health", http.StatusInternalServerError)
			return
		}

		info, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			slog.Error("[GetHealth] failed to get chain info", "error", err)
			http.Error(w, "Failed to get chain info for health", http.StatusInternalServerError)
			return
		}

		_, next := info.ExpectedNext()
		if next-2 > latest.Round {
			// we force a retry with another backend if we see a discrepancy in case that backend is stuck on a old latest beacon
			slog.Debug("[GetHealth] forcing retry with other SubConn")
			ctx := context.WithValue(r.Context(), grpc.SkipCtxKey{}, true)
			latest, err = c.GetBeacon(ctx, m, 0)
			if err != nil {
				slog.Error("[GetHealth] failed to get latest beacon", "error", err)
				http.Error(w, "Failed to get latest beacon for health", http.StatusInternalServerError)
				return
			}
		}

		if latest.Round >= next-2 {
			w.WriteHeader(http.StatusOK)
		} else {
			slog.Debug("[GetHealth] http.StatusServiceUnavailable", "current", latest.Round, "expected", next-1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		resp := make(map[string]uint64)
		resp["current"] = latest.Round
		resp["expected"] = next - 1

		json, err := json.Marshal(resp)
		if err != nil {
			slog.Error("[GetHealth] unable to encode HealthStatus in json", "error", err)
			http.Error(w, "Failed to encode HealthStatus", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetBeaconIds(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ids, _, err := c.GetBeaconIds(r.Context())
		if err != nil {
			slog.Error("[GetBeaconIds] failed to get beacon ids from client", "error", err)
			http.Error(w, "Failed to get beacon ids", http.StatusServiceUnavailable)
			return
		}

		json, err := json.Marshal(ids)
		if err != nil {
			slog.Error("[GetBeaconIds] failed to encode beacon ids in json", "error", err)
			http.Error(w, "Failed to produce beacon ids", http.StatusInternalServerError)
		}
		w.Write(json)
	}
}

func GetInfoV1(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetInfoV1] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			if err != nil {
				slog.Error("[GetInfoV1] failed to get ChainInfo from all clients", "error", err)
				http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
				return
			}
		}

		json, err := json.Marshal(chains.V1())
		if err != nil {
			slog.Error("[GetInfoV1] unable to encode ChainInfo in json", "error", err)
			http.Error(w, "Failed to encode ChainInfo", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetInfoV2(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetInfoV2] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			if err != nil {
				slog.Error("[GetInfoV2] failed to get ChainInfo", "error", err)
				http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
				return
			}
		}

		json, err := json.Marshal(chains)
		if err != nil {
			slog.Error("[GetInfoV2] unable to encode ChainInfo in json", "error", err)
			http.Error(w, "Failed to encode ChainInfo", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetLatest(c *grpc.Client, isV2 bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetLatest] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get latest", http.StatusInternalServerError)
			return
		}

		beacon, err := c.GetBeacon(r.Context(), m, 0)
		if err != nil {
			if err != nil {
				slog.Error("[GetLatest] unable to get beacon from any grpc client", "error", err)
				http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
				return
			}
		}

		// TODO: should we rather use the api.version key from the request context set in apiVersionCtx?
		// the current way of doing it probably allows the compiler to inline the right path tho...
		if isV2 {
			// we make sure that the V2 api aren't marshaling randommness
			beacon.UnsetRandomness()
		} else {
			// we need to set the randomness since the nodes are not supposed to send it over the wire anymore
			beacon.SetRandomness()
		}

		json, err := json.Marshal(beacon)
		if err != nil {
			slog.Error("[GetLatest] unable to encode beacon in json", "error", err)
			http.Error(w, "Failed to encode beacon", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetNext(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetNext] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get latest", http.StatusInternalServerError)
			return
		}

		beacon, err := c.Next(r.Context(), m)
		if err != nil {
			if err != nil {
				slog.Error("[GetNext] unable to get next beacon from any grpc client", "error", err)
				http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
				return
			}
		}

		json, err := json.Marshal(beacon)
		if err != nil {
			slog.Error("[GetNext] unable to encode beacon in json", "error", err)
			http.Error(w, "Failed to encode beacon", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func createRequestMD(r *http.Request) (*proto.Metadata, error) {
	chainhash := chi.URLParam(r, "chainhash")
	beaconID := chi.URLParam(r, "beaconID")

	// handling the default case
	if chainhash == "" && beaconID == "" {
		return &proto.Metadata{BeaconID: common.DefaultBeaconID}, nil
	}

	// warning when unusual request is built
	if len(chainhash) == 64 && beaconID != "" {
		slog.Warn("[createRequestMD] unexpectedly, createRequestMD got both a chainhash and a beaconID. Ignoring beaconID")
	}

	// handling the beacon ID case
	if beaconID != "" && chainhash == "" {
		return &proto.Metadata{BeaconID: beaconID}, nil
	}

	// handling the chain hash case
	hash, err := hex.DecodeString(chainhash)
	if err != nil {
		slog.Error("[createRequestMD] error decoding hex", "chainhash", chainhash, "error", err)
		return nil, errors.New("unable to decode chainhash as hex")
	}

	return &proto.Metadata{ChainHash: hash}, nil
}
