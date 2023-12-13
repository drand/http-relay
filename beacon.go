package main

import (
	"encoding/hex"

	"github.com/drand/http-server/grpc"
)

type HexBeacon struct {
	Round             uint64 `json:"round"`
	Signature         string `json:"signature"`
	PreviousSignature string `json:"previous_signature,omitempty"`
	Randomness        string `json:"randomness,omitempty"`
}

func NewHexBeacon(beacon grpc.RandomData) *HexBeacon {
	return &HexBeacon{
		Round:             beacon.GetRound(),
		Signature:         hex.EncodeToString(beacon.GetSignature()),
		PreviousSignature: hex.EncodeToString(beacon.GetPreviousSignature()),
		Randomness:        hex.EncodeToString(beacon.GetRandomness()),
	}
}
