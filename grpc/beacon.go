package grpc

import (
	"encoding/hex"
	"encoding/json"
)

// HexBeacon is a struct that get marshaled into hex-encoded signatures and randomness in JSON
type HexBeacon struct {
	Round             uint64   `json:"round"`
	Signature         HexBytes `json:"signature"`
	PreviousSignature HexBytes `json:"previous_signature,omitempty"`
	Randomness        HexBytes `json:"randomness,omitempty"`
}

type RandomData interface {
	GetRound() uint64
	GetRandomness() []byte
	GetSignature() []byte
	GetPreviousSignature() []byte
}

func NewHexBeacon(beacon RandomData) *HexBeacon {
	return &HexBeacon{
		Round:             beacon.GetRound(),
		Signature:         beacon.GetSignature(),
		PreviousSignature: beacon.GetPreviousSignature(),
		Randomness:        beacon.GetRandomness(),
	}
}

// HexBytes ensures that JSON marshallers marshal to hex rather than base64 to keep compatibility
// with old store formats
type HexBytes []byte

func (h *HexBytes) MarshalJSON() ([]byte, error) {
	hexString := hex.EncodeToString(*h)
	return json.Marshal(hexString)
}

// UnmarshalJSON converts a hexadecimal string from JSON to a byte slice
func (h *HexBytes) UnmarshalJSON(data []byte) error {
	var hexString string
	if err := json.Unmarshal(data, &hexString); err != nil {
		return err
	}

	b, err := hex.DecodeString(hexString)
	if err != nil {
		return err
	}

	*h = b
	return nil
}
