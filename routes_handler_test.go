package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/drand/drand/v2/common"
	"github.com/go-chi/chi/v5"
)

func TestCreateRequestMD(t *testing.T) {
	const quicknet = "52db9ba70e0cc0f6eaf7803dd07447a1f5477735fd3f661792ba94600c84e971"
	quicknetHash, err := hex.DecodeString(quicknet)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		chainhash     string
		beaconID      string
		wantBeaconID  string
		wantChainHash []byte
	}{
		{name: "default", wantBeaconID: common.DefaultBeaconID},
		{name: "beacon id", beaconID: "quicknet", wantBeaconID: "quicknet"},
		{name: "chain hash", chainhash: quicknet, wantChainHash: quicknetHash},
		{name: "chain hash as beacon id", beaconID: quicknet, wantChainHash: quicknetHash},
		{name: "64 non-hex chars as beacon id", beaconID: string(bytes.Repeat([]byte("z"), 64)), wantBeaconID: string(bytes.Repeat([]byte("z"), 64))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("chainhash", tt.chainhash)
			rctx.URLParams.Add("beaconID", tt.beaconID)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

			md, err := createRequestMD(r)
			if err != nil {
				t.Fatalf("createRequestMD() error = %v", err)
			}
			if md.BeaconID != tt.wantBeaconID {
				t.Errorf("createRequestMD().BeaconID = %q, want %q", md.BeaconID, tt.wantBeaconID)
			}
			if !bytes.Equal(md.ChainHash, tt.wantChainHash) {
				t.Errorf("createRequestMD().ChainHash = %x, want %x", md.ChainHash, tt.wantChainHash)
			}
		})
	}
}
