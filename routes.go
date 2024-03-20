package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
)

func DisplayRoutes(allRoutes []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		w.Write(allRoutes)
	}
}

func GetBeacon(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
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

		beacon, err := c.GetBeacon(r.Context(), m, round)
		if err != nil {
			w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
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
			info, err := c.GetChainInfo(r.Context(), m)
			if err != nil {
				slog.Error("[GetBeacon] error retrieving chain info", "error", err)
				// we skip cache-age setting
				w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
			}

			cacheTime, expected, err := NextBeaconTime(info, beacon.Round)
			if err != nil {
				w.Header().Set("Cache-Control", "must-revalidate, no-cache, max-age=0")
				slog.Error("[GetBeacon] Future beacon was received, unexpected", "latest", beacon.Round, "expected", expected)
			} else {
				w.Header().Set("Cache-Control",
					fmt.Sprintf("public, must-revalidate, max-age=%d", cacheTime))
				slog.Info("[GetBeacon] StatusOK", "cachetime", cacheTime)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(json)
	}
}

func NextBeaconTime(info *grpc.JsonInfoV2, currentRound uint64) (int64, uint64, error) {
	p := int64(info.Period)
	// we rely on integer division rounding down, plus one because round 1 happened at GenesisTime
	expected := ((time.Now().Unix() - info.GenesisTime) / p) + 1
	if currentRound >= uint64(expected) {
		return 0, uint64(expected), fmt.Errorf("outdated round")
	}
	return expected*p + info.GenesisTime - time.Now().Unix(), uint64(expected), nil
}

func GetChains(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		chains, err := c.GetChains(r.Context())
		if err != nil {
			slog.Error("failed to get chains", "error", err)
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
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get health", http.StatusInternalServerError)
			return
		}

		latest, err := c.GetBeacon(r.Context(), m, 0)
		if err != nil {
			slog.Error("[GetHealth] failed to get latest beacon", "error", err)
			http.Error(w, "Failed to get latest beacon", http.StatusInternalServerError)
			return
		}

		info, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			slog.Error("[GetHealth] failed to get chain info", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		_, expected, err := NextBeaconTime(info, latest.Round)
		if err != nil {
			slog.Error("NextBeaconTime", "error", err)
			http.Error(w, "Failed to encode HealthStatus", http.StatusInternalServerError)

		}

		if expected == latest.Round {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		resp := make(map[string]uint64)
		resp["current"] = latest.Round
		resp["expected"] = expected

		json, err := json.Marshal(resp)
		if err != nil {
			slog.Error("unable to encode HealthStatus in json", "error", err)
			http.Error(w, "Failed to encode HealthStatus", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetInfoV1(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			slog.Error("failed to get ChainInfo", "error", err)
			http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
			return
		}

		json, err := json.Marshal(chains.V1())
		if err != nil {
			slog.Error("unable to encode ChainInfo in json", "error", err)
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
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(r.Context(), m)
		if err != nil {
			slog.Error("failed to get ChainInfo", "error", err)
			http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
			return
		}

		json, err := json.Marshal(chains)
		if err != nil {
			slog.Error("unable to encode ChainInfo in json", "error", err)
			http.Error(w, "Failed to encode ChainInfo", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetLatest(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := createRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get latest", http.StatusInternalServerError)
			return
		}

		beacon, err := c.GetBeacon(r.Context(), m, 0)
		if err != nil {
			slog.Error("unable to get beacon from grpc client", "error", err)
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}

		json, err := json.Marshal(beacon)
		if err != nil {
			slog.Error("unable to encode beacon in json", "error", err)
			http.Error(w, "Failed to encode beacon", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func createRequestMD(r *http.Request) (*proto.Metadata, error) {
	chainhash := chi.URLParam(r, "chainhash")
	beaconID := chi.URLParam(r, "beaconID")

	if chainhash == "" && beaconID == "" {
		return &proto.Metadata{BeaconID: "default"}, nil
	}

	if len(chainhash) == 64 && beaconID != "" {
		slog.Warn("[createRequestMD] unexpectedly, createRequestMD got both a chainhash and a beaconID. Ignoring beaconID")
	}

	if beaconID != "" && chainhash == "" {
		return &proto.Metadata{BeaconID: beaconID}, nil
	}

	hash, err := hex.DecodeString(chainhash)
	if err != nil {
		slog.Error("[createRequestMD] error decoding hex", "chainhash", chainhash, "error", err)
		return nil, errors.New("unable to decode chainhash as hex")
	}

	return &proto.Metadata{ChainHash: hash}, nil
}
