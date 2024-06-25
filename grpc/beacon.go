package grpc

import (
	"encoding/hex"
	"encoding/json"

	"github.com/drand/drand/v2/crypto"
)

// HexBeacon is a struct that get marshaled into hex-encoded signatures and randomness in JSON
type HexBeacon struct {
	Round             uint64   `json:"round"`
	Randomness        HexBytes `json:"randomness,omitempty"`
	Signature         HexBytes `json:"signature"`
	PreviousSignature HexBytes `json:"previous_signature,omitempty"`
}

func (h *HexBeacon) GetRound() uint64 {
	return h.Round
}

func (h *HexBeacon) SetRandomness() {
	h.Randomness = crypto.RandomnessFromSignature(h.Signature)
}

func (h *HexBeacon) UnsetRandomness() {
	h.Randomness = nil
}

func (h *HexBeacon) GetRandomness() []byte {
	return h.Randomness
}

func (h *HexBeacon) GetSignature() []byte {
	return h.Signature
}
func (h *HexBeacon) GetPreviousSignature() []byte {
	return h.PreviousSignature
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
	}
}

// HexBytes ensures that JSON marshallers marshal to hex rather than base64 to keep compatibility
// with old store formats
type HexBytes []byte

func (h *HexBytes) String() string {
	return hex.EncodeToString(*h)
}

func (h *HexBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.String())
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
