package grpc

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeBeacon(t *testing.T) {
	sSig := "9469186f38e5acdac451940b1b22f737eb0de060b213f0326166c7882f2f82b92ce119bdabe385941ef46f72736a4b4d02ce206e1eb46cac53019caf870080fede024edcd1bd0225eb1335b83002ae1743393e83180e47d9948ab8ba7568dd99"
	bSig, err := hex.DecodeString(sSig)
	require.NoError(t, err)
	sPrev := "a418fccbfaa0c84aba8cbcd4e3c0555170eb2382dfed108ecfc6df249ad43efe00078bdcb5060fe2deed4731ca5b4c740069aaf77927ba59c5870ab3020352aca3853adfdb9162d40ec64f71b121285898e28cdf237e982ac5c4deb287b0d57b"
	bPrev, err := hex.DecodeString(sPrev)
	require.NoError(t, err)
	sRand := "a9f12c5869d05e084d1741957130e1d0bf78a8ca9a8deb97c47cac29aae433c6"
	bRand, err := hex.DecodeString(sRand)
	require.NoError(t, err)

	beacon := &proto.PublicRandResponse{
		Round:             123,
		Signature:         bSig,
		Randomness:        bRand,
		PreviousSignature: bPrev,
	}
	json, err := json.Marshal(NewHexBeacon(beacon))
	require.NoError(t, err)
	assert.Contains(t, string(json), sSig)
	assert.Contains(t, string(json), "\"signature\":")
	assert.Contains(t, string(json), sPrev)
	assert.Contains(t, string(json), "\"previous_signature\":")
	assert.Contains(t, string(json), sRand)
	assert.Contains(t, string(json), "\"randomness\":")
}

func TestUmarshal(t *testing.T) {
	jsonStr := `{"round":699349,"randomness":"19dcce6d274878de3256272cb2476e82f1d0ae197e86fb076ba7537c89e66cc0","signature":"8ca93e637e34c1ac7f7f3342e76c3c876091a68d16e794c53b54f456d366860021de57eb55f6a81ccc58c8b89e0d793e0717628b9b96552433516d9d55932607f25b71ca60e32bef258a746854ff752c4666dc0c65a96a499331ac8ad4207012","previous_signature":"b8cffba5fe405384c79a4eb6a6287ce3345228b06c398d1bb1836002b48a738247150295c60044c823454fb9d6b06b870065c84c18ce5862964c0566662c83e8e92ba62799cb40d126b0b69556233b70238d834e9c130e34166fa200fcba859d"}`
	sRand := "19dcce6d274878de3256272cb2476e82f1d0ae197e86fb076ba7537c89e66cc0"
	bRand, err := hex.DecodeString(sRand)
	require.NoError(t, err)

	sSig := "8ca93e637e34c1ac7f7f3342e76c3c876091a68d16e794c53b54f456d366860021de57eb55f6a81ccc58c8b89e0d793e0717628b9b96552433516d9d55932607f25b71ca60e32bef258a746854ff752c4666dc0c65a96a499331ac8ad4207012"
	bSig, err := hex.DecodeString(sSig)
	require.NoError(t, err)
	beacon := new(HexBeacon)
	err = json.Unmarshal([]byte(jsonStr), beacon)
	require.NoError(t, err)
	require.Equal(t, uint64(699349), beacon.Round)
	require.Equal(t, bRand, beacon.GetRandomness())
	require.Equal(t, bSig, beacon.GetSignature())
	// computes and overwrite the randomness value
	beacon.SetRandomness()
	require.Equal(t, beacon.GetRandomness(), bRand)
}

func TestEncodeWeirdBeacons(t *testing.T) {
	tests := []struct {
		name   string
		beacon *proto.PublicRandResponse
	}{
		{
			name:   "all empty",
			beacon: &proto.PublicRandResponse{},
		},
		{
			name:   "nil proto",
			beacon: nil,
		},
		{
			name: "all nil",
			beacon: &proto.PublicRandResponse{
				Round:             0,
				Signature:         nil,
				Randomness:        nil,
				PreviousSignature: nil,
			},
		},
		{
			name: "all empty slice round 1",
			beacon: &proto.PublicRandResponse{
				Round:             1,
				Signature:         []byte{},
				Randomness:        []byte{},
				PreviousSignature: []byte{}},
		}, {
			name: "all empty string round max uint + 1",
			beacon: &proto.PublicRandResponse{
				Round:             4294967296,
				Signature:         []byte(""),
				Randomness:        []byte(""),
				PreviousSignature: []byte("")},
		},
		{
			name: "omitempty previous signature",
			beacon: &proto.PublicRandResponse{
				Round:      1,
				Signature:  []byte{},
				Randomness: []byte{}},
		},
		{
			name: "all omitempty",
			beacon: &proto.PublicRandResponse{
				Round:     1,
				Signature: []byte{}},
		},
		{
			name:   "only randomness",
			beacon: &proto.PublicRandResponse{Randomness: []byte("test")},
		},
		{
			name: "filled with strings",
			beacon: &proto.PublicRandResponse{
				Round:             1,
				Signature:         []byte("strings"),
				Randomness:        []byte("strings"),
				PreviousSignature: []byte("strings")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			json, err := json.Marshal(NewHexBeacon(test.beacon))
			require.NoError(t, err)
			assert.Contains(t, string(json), "\"signature\":")
			assert.Contains(t, string(json), "\"round\":")
			if len(test.beacon.GetPreviousSignature()) > 0 {
				assert.Contains(t, string(json), "\"previous_signature\":")
			} else {
				assert.NotContains(t, string(json), "\"previous_signature\":")
			}
			if len(test.beacon.GetRandomness()) > 0 {
				assert.Contains(t, string(json), "\"randomness\":")
			} else {
				assert.NotContains(t, string(json), "\"randomness\":")
			}
		})
	}
}

func TestHexBytes_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected HexBytes
		wantErr  bool
	}{
		{
			name:     "empty",
			input:    []byte{34, 34},
			expected: HexBytes{},
			wantErr:  false,
		}, {
			name:     "zero",
			input:    []byte{34, 48, 48, 48, 48, 34},
			expected: HexBytes{0, 0},
			wantErr:  false,
		}, {
			name:     "A",
			input:    []byte{34, 65, 65, 34},
			expected: HexBytes{170},
			wantErr:  false,
		}, {
			name:     "uneven",
			input:    []byte{42, 42, 42},
			expected: HexBytes{},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := HexBytes{}
			err := res.UnmarshalJSON(tt.input)
			if err != nil {
				if !tt.wantErr {
					t.Fatalf("unable to unmarshal %s on input %v: %v", tt.name, tt.input, err)
				}
			} else if tt.wantErr {
				t.Fatalf("nil error on input %v, got %x. Expected error.", tt.input, res)
			}

			assert.Equal(t, tt.expected, res)
		})
	}
}

func TestHexBytes_MarshalJSON(t *testing.T) {
	tests := []struct {
		name   string
		output []byte
		h      HexBytes
	}{
		{
			name:   "empty",
			output: []byte{34, 34},
			h:      HexBytes{},
		}, {
			name:   "zero",
			output: []byte{34, 48, 48, 34},
			h:      HexBytes{0},
		}, {
			name:   "zeros",
			output: []byte{34, 48, 48, 48, 48, 48, 48, 34},
			h:      HexBytes{0, 0, 0},
		}, {
			name:   "one",
			output: []byte{34, 48, 49, 34},
			h:      HexBytes{1},
		}, {
			name:   "ones",
			output: []byte{34, 48, 49, 48, 49, 34},
			h:      HexBytes{1, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.h.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, tt.output, got)
		})
	}
}
