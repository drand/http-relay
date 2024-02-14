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

	proto "github.com/drand/drand/protobuf/drand"
	"github.com/drand/http-server/grpc"
	"github.com/go-chi/chi/v5"
)

func GetBeacon(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := CreateRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get health", http.StatusInternalServerError)
			return
		}

		roundStr := chi.URLParam(r, "round")
		round, err := strconv.ParseUint(roundStr, 10, 64)
		if err != nil {
			http.Error(w, "Failed to parse round. Err: "+err.Error(), http.StatusBadRequest)
			return
		}

		beacon, err := c.GetBeacon(m, round)
		if err != nil {
			http.Error(w, "Failed to get beacon", http.StatusInternalServerError)
			return
		}

		json, err := json.Marshal(beacon)
		if err != nil {
			http.Error(w, "Failed to Encode beacon in hex", http.StatusInternalServerError)
			return
		}

		w.Write(json)
	}
}

func GetChains(c *grpc.Client) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		chains, err := c.GetChains()
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
		m, err := CreateRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get health", http.StatusInternalServerError)
			return
		}

		latest, info, err := c.GetHealth(m)
		if err != nil {
			slog.Error("[GetHealth] failed to get chain info", "error", err)
			http.Error(w, "Failed to get chains for info", http.StatusServiceUnavailable)
			return
		}

		p := int64(info.GetPeriod())
		if p == 0 {
			slog.Error("[GetHealth] got invalid period = 0")
			http.Error(w, "Failed to get health", http.StatusInternalServerError)
			return
		}

		// we rely on integer division rounding down, plus one because round 1 happened at GenesisTime
		expected := ((time.Now().Unix() - info.GetGenesisTime()) / p) + 1

		if latest == uint64(expected) || latest+1 == uint64(expected) {
			cacheTime := expected*p + info.GetGenesisTime() - time.Now().Unix()
			w.Header().Set("Cache-Control",
				fmt.Sprintf("public, must-revalidate, max-age=%d", cacheTime))
			w.WriteHeader(http.StatusOK)
			slog.Info("[GetHealth] StatusOK", "cachetime", cacheTime)

		} else {
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusServiceUnavailable)
			slog.Error("[GetHealth] StatusServiceUnavailable", "latest", latest, "expected", expected)
		}

		resp := make(map[string]uint64)
		resp["current"] = latest
		resp["expected"] = uint64(expected)

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
		m, err := CreateRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(m)
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
		m, err := CreateRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get info", http.StatusInternalServerError)
			return
		}

		chains, err := c.GetChainInfo(m)
		if err != nil {
			slog.Error("failed to get ChainInfo", "error", err)
			http.Error(w, "Failed to get ChainInfo", http.StatusInternalServerError)
			return
		}

		json, err := json.Marshal(chains.V2())
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
		m, err := CreateRequestMD(r)
		if err != nil {
			slog.Error("unable to create metadata for request", "error", err)
			http.Error(w, "Failed to get latest", http.StatusInternalServerError)
			return
		}

		beacon, err := c.GetBeacon(m, 0)
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

func CreateRequestMD(r *http.Request) (*proto.Metadata, error) {
	chainhash := chi.URLParam(r, "chainhash")
	beaconID := chi.URLParam(r, "beaconID")

	if chainhash == "" && (beaconID == "" || beaconID == "public") {
		return &proto.Metadata{BeaconID: "default"}, nil
	}

	if len(chainhash) == 64 && beaconID != "" {
		slog.Warn("[CreateRequestMD] unexpectedly, CreateRequestMD got both a chainhash and a beaconID. Ignoring beaconID")
	}

	if beaconID != "" && chainhash == "" {
		return &proto.Metadata{BeaconID: beaconID}, nil
	}

	hash, err := hex.DecodeString(chainhash)
	if err != nil {
		slog.Error("[CreateRequestMD] error decoding hex", "chainhash", chainhash, "error", err)
		return nil, errors.New("unable to decode chainhash as hex")
	}

	return &proto.Metadata{ChainHash: hash}, nil
}
