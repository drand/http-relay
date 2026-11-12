// Package fakenode runs an in-process drand node over gRPC for tests and
// benchmarks. It emits a new beacon every period and records when each round
// was emitted so callers can measure emit-to-delivery latency.
package fakenode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net"
	"sync"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const (
	// BeaconID is the single beacon served by a Node.
	BeaconID = "default"
	// Period is the beacon period reported in the chain info.
	Period = time.Second
)

// Options configures a Node.
type Options struct {
	// RTT is added to every unary response and stream message to simulate network delay.
	RTT time.Duration
}

// Node is a fake drand node serving one chain.
type Node struct {
	proto.UnimplementedPublicServer

	opts     Options
	info     *proto.ChainInfoPacket
	server   *grpc.Server
	listener net.Listener
	stop     chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	beacons  []*proto.PublicRandResponse
	emitted  []time.Time
	watchers map[chan *proto.PublicRandResponse]struct{}
}

// Start listens on a random loopback port and begins emitting rounds. Round 1
// is emitted immediately; genesis is set so that round 1 is the current round.
func Start(opts Options) (*Node, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	hash := sha256.Sum256([]byte("fakenode-" + listener.Addr().String()))
	n := &Node{
		opts: opts,
		info: &proto.ChainInfoPacket{
			PublicKey:   randomBytes(96),
			Period:      uint32(Period / time.Second),
			GenesisTime: time.Now().Unix(),
			Hash:        hash[:],
			GroupHash:   randomBytes(32),
			SchemeID:    "bls-unchained-g1-rfc9380",
			Metadata:    &proto.Metadata{BeaconID: BeaconID, ChainHash: hash[:]},
		},
		server:   grpc.NewServer(),
		listener: listener,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		watchers: make(map[chan *proto.PublicRandResponse]struct{}),
	}
	proto.RegisterPublicServer(n.server, n)
	healthgrpc.RegisterHealthServer(n.server, health.NewServer())

	n.emit()
	go n.run()
	go n.server.Serve(listener) //nolint:errcheck // Serve returns once Stop closes the listener

	return n, nil
}

// Addr returns the host:port the node listens on.
func (n *Node) Addr() string { return n.listener.Addr().String() }

// ChainHash returns the chain hash of the served chain.
func (n *Node) ChainHash() []byte { return n.info.Hash }

// Stop shuts the node down and waits for the emitter to exit.
func (n *Node) Stop() {
	close(n.stop)
	<-n.done
	n.server.Stop()
}

// Round returns the latest emitted round.
func (n *Node) Round() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return uint64(len(n.beacons))
}

// EmitTime returns when the given round was emitted. It panics for rounds not
// yet emitted.
func (n *Node) EmitTime(round uint64) time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.emitted[round-1]
}

func (n *Node) run() {
	defer close(n.done)
	ticker := time.NewTicker(Period)
	defer ticker.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-ticker.C:
			n.emit()
		}
	}
}

func (n *Node) emit() {
	n.mu.Lock()
	defer n.mu.Unlock()

	b := &proto.PublicRandResponse{
		Round:     uint64(len(n.beacons) + 1),
		Signature: randomBytes(48),
		Metadata:  n.info.Metadata,
	}
	if len(n.beacons) > 0 {
		b.PreviousSignature = n.beacons[len(n.beacons)-1].Signature
	}
	n.beacons = append(n.beacons, b)
	n.emitted = append(n.emitted, time.Now())

	for ch := range n.watchers {
		select {
		case ch <- b:
		default: // slow watcher, it misses this round
		}
	}
}

func (n *Node) subscribe() (chan *proto.PublicRandResponse, func()) {
	ch := make(chan *proto.PublicRandResponse, 1)
	n.mu.Lock()
	n.watchers[ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.watchers, ch)
		n.mu.Unlock()
	}
}

func (n *Node) delay(ctx context.Context) error {
	if n.opts.RTT == 0 {
		return nil
	}
	select {
	case <-time.After(n.opts.RTT):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PublicRand serves round 0 as the latest beacon, blocks for the round right
// after the latest, and rejects rounds further in the future like drand does.
func (n *Node) PublicRand(ctx context.Context, in *proto.PublicRandRequest) (*proto.PublicRandResponse, error) {
	if err := n.delay(ctx); err != nil {
		return nil, err
	}

	n.mu.Lock()
	latest := uint64(len(n.beacons))
	round := in.GetRound()
	if round == 0 {
		round = latest
	}
	if round <= latest {
		b := n.beacons[round-1]
		n.mu.Unlock()
		return b, nil
	}
	n.mu.Unlock()

	if round > latest+1 {
		return nil, status.Errorf(codes.NotFound, "round %d is in the future (latest %d)", round, latest)
	}

	ch, unsubscribe := n.subscribe()
	defer unsubscribe()
	select {
	case b := <-ch:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// PublicRandStream streams every beacon emitted after the call, matching the
// drand behaviour for round 0 that the relay's Watch relies on.
func (n *Node) PublicRandStream(_ *proto.PublicRandRequest, stream proto.Public_PublicRandStreamServer) error {
	ch, unsubscribe := n.subscribe()
	defer unsubscribe()
	ctx := stream.Context()
	for {
		select {
		case b := <-ch:
			if err := n.delay(ctx); err != nil {
				return err
			}
			if err := stream.Send(b); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ChainInfo returns the single chain's info for any metadata.
func (n *Node) ChainInfo(ctx context.Context, _ *proto.ChainInfoRequest) (*proto.ChainInfoPacket, error) {
	if err := n.delay(ctx); err != nil {
		return nil, err
	}
	return n.info, nil
}

// ListBeaconIDs lists the single served beacon.
func (n *Node) ListBeaconIDs(ctx context.Context, _ *proto.ListBeaconIDsRequest) (*proto.ListBeaconIDsResponse, error) {
	if err := n.delay(ctx); err != nil {
		return nil, err
	}
	return &proto.ListBeaconIDsResponse{
		Ids:       []string{BeaconID},
		Metadatas: []*proto.Metadata{n.info.Metadata},
	}, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
