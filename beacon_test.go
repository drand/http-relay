package main

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	proto "github.com/drand/drand/protobuf/drand"
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
