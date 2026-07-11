package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/drand/drand/v2/common"
	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/http-relay/grpc"
	"github.com/go-chi/chi/v5"
)

func GetBeacon(c *grpc.Client, isV2 bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		roundStr := chi.URLParam(r, "round")
		round, err := strconv.ParseUint(roundStr, 10, 64)
		if err != nil {
			w.Header().Set("Cache-Control", "public, max-age=604800, immutable")

			slog.Error("unable to parse round", "error", err)
			http.Error(w, fmt.Sprintf("Failed to parse round parameter %q: %v", roundStr, err), http.StatusBadRequest)
			return
		}

		beacon, nextTime, err := getBeacon(c, r, round)
		if err != nil {
			slog.Error("Failed get beacon", "error", err, "nextTime", nextTime)

			if nextTime < 0 {
				w.Header().Set("Cache-Control", fmt.Sprintf("must-revalidate, public, max-age=%d", -nextTime))

				// I know, 425 is meant to indicate a replay attack risk, but hey, it's the perfect error name!
				http.Error(w, "Requested future beacon", http.StatusTooEarly)
			} else {
				w.Header().Set("Cache-Control", "no-cache")

				http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			}
			return
		}

		writeBeacon(w, beacon, nextTime, isV2)
	}
}

// getBeacon return the HexBeacon, the time of the next round, and/or an error.
// A negative nextTime value is only used in case of an error, to indicate how
// long that error should be cached.
func getBeacon(c *grpc.Client, r *http.Request, round uint64) (*grpc.HexBeacon, int64, error) {
	m, err := createRequestMD(r)
	if err != nil {
		return nil, 0, fmt.Errorf("createRequestMD error: %w", err)
	}

	info, err := c.GetChainInfo(r.Context(), m)
	if err != nil {
		return nil, 0, fmt.Errorf("GetChainInfo error: %w", err)
	}

	nextTime, nextRound := info.ExpectedNext()
	// we refuse rounds too far in the future
	if round >= nextRound+1 {
		slog.Error("[GetBeacon] Future beacon was requested, unexpected", "requested", round, "expected", nextRound, "from", r.RemoteAddr)
		// Use precise time until next round for cache duration instead of full period
		preciseCacheSeconds := info.SecondsUntilRound(nextRound)
		return nil, -preciseCacheSeconds, fmt.Errorf("future beacon was requested")
	}

	var beacon *grpc.HexBeacon
	// if we are requesting the next round
	if round == nextRound {
		beacon, err = c.Next(r.Context(), m)
		if err != nil {
			slog.Error("[GetBeacon] unable to get next beacon from any grpc client", "error", err)
			return nil, 0, fmt.Errorf("Next error: %w", err)

		}
		// we use -1 to indicate no caching
		nextTime = -1
		if beacon.GetRound() != round {
			beacon, err = c.GetBeacon(r.Context(), m, round)
			if err != nil {
				slog.Error("all clients are unable to provide beacons", "error", err)
				return nil, 0, fmt.Errorf("GetBeacon error: %w", err)
			}
			nextTime = 0
		}
	} else {
		beacon, err = c.GetBeacon(r.Context(), m, round)
		if err != nil {
			slog.Error("all clients are unable to provide beacons", "error", err)
			return nil, 0, fmt.Errorf("GetBeacon error: %w", err)
		}
		// if this was a query for a set round, we cache it forever anyway
		if round != 0 {
			nextTime = 0
		}
	}
	return beacon, nextTime, nil
}

func writeBeacon(w http.ResponseWriter, beacon *grpc.HexBeacon, nextTime int64, isV2 bool) {
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
		w.Header().Set("Cache-Control", "no-cache")

		slog.Error("unable to encode beacon in json", "error", err)
		http.Error(w, "Failed to encode beacon", http.StatusInternalServerError)
		return
	}

	if nextTime == 0 {
		// i.e. we're not fetching latest or next, we can store these beacons for a long time
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	} else if nextTime < 0 {
		// we must never cache the next beacon, since we wait for them
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		// for latest we compute the right time
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

func GetLatest(c *grpc.Client, isV2 bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		beacon, nextTime, err := getBeacon(c, r, 0)
		if err != nil {
			w.Header().Set("Cache-Control", "no-cache")

			slog.Error("Failed get beacon", "error", err, "nextTime", nextTime)
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}

		writeBeacon(w, beacon, nextTime, isV2)
	}
}

func GetNext(c *grpc.Client, isV2 bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("[GetNext] unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get latest", http.StatusInternalServerError)
			return
		}

		beacon, err := c.Next(r.Context(), m)
		if err != nil {
			slog.Error("[GetNext] unable to get next beacon from any grpc client", "error", err)
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}
		// GetNext is only available as a V2 endpoint
		// -1 because no caching
		writeBeacon(w, beacon, -1, isV2)
	}
}

func GetChains(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		chains, err := c.GetChains(r.Context())
		if err != nil {
			slog.Error("failed to get chains from all clients", "error", err)
			http.Error(w, "Failed to get chains", http.StatusInternalServerError)
			return
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
			return
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
			slog.Error("[GetInfoV1] failed to get ChainInfo from all clients", "error", err)
			http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
			return
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
			slog.Error("[GetInfoV2] failed to get ChainInfo", "error", err)
			http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
			return
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
