package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/drand/drand/v2/common"
	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/http-relay/grpc"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	ggrpc "google.golang.org/grpc"
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
		wantErr       bool
	}{
		{name: "default", wantBeaconID: common.DefaultBeaconID},
		{name: "beacon id", beaconID: "quicknet", wantBeaconID: "quicknet"},
		{name: "chain hash", chainhash: quicknet, wantChainHash: quicknetHash},
		{name: "chain hash as beacon id", beaconID: quicknet, wantChainHash: quicknetHash},
		{name: "64 non-hex chars as beacon id", beaconID: string(bytes.Repeat([]byte("z"), 64)), wantBeaconID: string(bytes.Repeat([]byte("z"), 64))},
		{name: "invalid UTF-8 beacon id", beaconID: "\xff", wantErr: true},
		{name: "invalid hex chain hash", chainhash: "zz", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("chainhash", tt.chainhash)
			rctx.URLParams.Add("beaconID", tt.beaconID)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

			md, err := createRequestMD(r)
			if tt.wantErr {
				if err == nil {
					t.Errorf("createRequestMD() = %v, want an error", md)
				}
				return
			}
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

// emptyNode is a drand node serving no chain.
type emptyNode struct {
	proto.UnimplementedPublicServer
}

func (emptyNode) ListBeaconIDs(context.Context, *proto.ListBeaconIDsRequest) (*proto.ListBeaconIDsResponse, error) {
	return &proto.ListBeaconIDsResponse{}, nil
}

// picks returns how many calls the balancer sent to addr.
func picks(t *testing.T, addr string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "grpc_client_requests_per_backend" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "node" && l.GetValue() == addr {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestInvalidBeaconID checks that a beacon ID gRPC cannot marshal never reaches the balancer, which would penalize
// the node it picked.
func TestInvalidBeaconID(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := ggrpc.NewServer()
	proto.RegisterPublicServer(srv, emptyNode{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	addr := lis.Addr().String()
	client, err := grpc.NewClient("fallback:///"+addr, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := chi.NewRouter()
	SetupRoutes(r, client)

	for _, path := range []string{"info", "health", "rounds/latest", "rounds/1", "rounds/next"} {
		t.Run(path, func(t *testing.T) {
			before := picks(t, addr)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/beacons/%FF/"+path, nil))
			if w.Code != http.StatusInternalServerError {
				t.Errorf("GET /v2/beacons/%%FF/%s = %d, want %d", path, w.Code, http.StatusInternalServerError)
			}
			if got := picks(t, addr) - before; got != 0 {
				t.Errorf("GET /v2/beacons/%%FF/%s picked a node %v times, want 0", path, got)
			}
		})
	}
}
