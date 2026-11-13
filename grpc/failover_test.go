package grpc

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// recordingBuilder builds fallback balancers under its own name and hands each one to built.
type recordingBuilder struct {
	balancer.Builder
	built chan *fallbackBalancer
}

func (recordingBuilder) Name() string { return "recording_" + fallbackName }

func (b recordingBuilder) Build(cc balancer.ClientConn, opts balancer.BuildOptions) balancer.Balancer {
	fb := b.Builder.Build(cc, opts).(*fallbackBalancer)
	b.built <- fb
	return fb
}

var (
	registerRecording sync.Once
	builtBalancers    = make(chan *fallbackBalancer, 1)
)

// streamNode is a drand node whose PublicRandStream fails with fail, or sends one beacon when fail is nil.
type streamNode struct {
	proto.UnimplementedPublicServer
	name   string
	fail   func(proto.Public_PublicRandStreamServer) error
	opened func(name string)
}

func (n *streamNode) ChainInfo(context.Context, *proto.ChainInfoRequest) (*proto.ChainInfoPacket, error) {
	return &proto.ChainInfoPacket{
		Period:   fakePeriod,
		Metadata: &proto.Metadata{BeaconID: "default", ChainHash: fakeChainHash},
	}, nil
}

func (n *streamNode) PublicRandStream(_ *proto.PublicRandRequest, stream proto.Public_PublicRandStreamServer) error {
	n.opened(n.name)
	if n.fail != nil {
		return n.fail(stream)
	}
	if err := stream.Send(&proto.PublicRandResponse{Round: 42}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func stallStream(stream proto.Public_PublicRandStreamServer) error {
	<-stream.Context().Done()
	return nil
}

// failStream fails like a node that knows the chain but has no beacon yet: the client gets codes.Unknown.
func failStream(proto.Public_PublicRandStreamServer) error {
	return errors.New("beacon has not started on this node yet")
}

func downStream(proto.Public_PublicRandStreamServer) error {
	return status.Error(codes.Unavailable, "node down")
}

// serveNodes serves one streamNode per failing function, with fail set to it, through a fallback balancer. It returns
// the client, the balancer, and a function listing the nodes that opened a stream, in order.
func serveNodes(t *testing.T, failing ...func(proto.Public_PublicRandStreamServer) error) (*Client, *fallbackBalancer, func() []string) {
	t.Helper()
	registerRecording.Do(func() {
		balancer.Register(recordingBuilder{NewFallbackBuilder(FallbackTimeout), builtBalancers})
	})
	var (
		mu     sync.Mutex
		opened []string
	)
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, name)
	}
	addrs := []string{"a", "b", "c", "d"}[:len(failing)]
	listeners := map[string]*bufconn.Listener{}
	for i, addr := range addrs {
		lis := bufconn.Listen(1 << 16)
		srv := grpc.NewServer()
		proto.RegisterPublicServer(srv, &streamNode{name: addr, fail: failing[i], opened: record})
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		listeners[addr] = lis
	}

	conn, err := grpc.NewClient("fallback:///"+strings.Join(addrs, ","),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"recording_`+fallbackName+`"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return listeners[addr].DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.Connect()
	fb := <-builtBalancers
	waitReady(t, fb, len(addrs))

	c := newClient(proto.NewPublicClient(conn), slog.New(slog.DiscardHandler))
	t.Cleanup(c.cancel)
	c.minBackoff, c.maxBackoff, c.nodes = time.Millisecond, 10*time.Millisecond, len(addrs)
	return c, fb, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(opened)
	}
}

// penalized returns the penalized nodes of fb, sorted.
func penalized(fb *fallbackBalancer) []string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	var addrs []string
	for addr, n := range fb.nodes {
		if fb.now().Before(n.penalizedUntil) {
			addrs = append(addrs, addr)
		}
	}
	slices.Sort(addrs)
	return addrs
}

func TestStreamReopensOnNextNode(t *testing.T) {
	tests := []struct {
		name          string
		failing       []func(proto.Public_PublicRandStreamServer) error
		wantPenalized []string
	}{
		{"stall", []func(proto.Public_PublicRandStreamServer) error{stallStream}, nil},
		{"error", []func(proto.Public_PublicRandStreamServer) error{failStream}, nil},
		{"node down", []func(proto.Public_PublicRandStreamServer) error{downStream}, []string{"a"}},
		{"two stalls", []func(proto.Public_PublicRandStreamServer) error{stallStream, stallStream}, nil},
		{"three stalls", []func(proto.Public_PublicRandStreamServer) error{stallStream, stallStream, stallStream}, nil},
		{"three failing nodes", []func(proto.Public_PublicRandStreamServer) error{stallStream, failStream, stallStream}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, fb, opened := serveNodes(t, append(tt.failing, nil)...)
			addrs := []string{"a", "b", "c", "d"}[:len(tt.failing)+1]
			ch := make(chan nextResult, 1)
			s := &sharedStream{waiters: map[chan nextResult]struct{}{ch: {}}, running: true, failed: map[string]struct{}{}}

			done := make(chan struct{})
			go func() {
				// well above scheduling delays, so that the healthy node sends its beacon before it stalls
				c.runStream(s, &proto.Metadata{}, 300*time.Millisecond)
				close(done)
			}()
			select {
			case res := <-ch:
				if res.err != nil || res.beacon.Round != 42 {
					t.Errorf("stream got %v, %v, want round 42", res.beacon, res.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stream got no beacon")
			}
			if got := penalized(fb); !slices.Equal(got, tt.wantPenalized) {
				t.Errorf("penalized nodes = %v, want %v", got, tt.wantPenalized)
			}
			c.cancel()
			<-done

			// only the last node sends beacons, a slow runner may still reopen the stream after it
			if got := opened(); len(got) < len(addrs) || !slices.Equal(got[:len(addrs)], addrs) {
				t.Errorf("streams opened on %v, want %v first", got, addrs)
			}
		})
	}
}

// TestNextStreamErrorOnEveryNode checks that a chain no node can stream yet fails Next after one attempt per node,
// without penalizing any node for the other chains.
func TestNextStreamErrorOnEveryNode(t *testing.T) {
	c, fb, opened := serveNodes(t, failStream, failStream, failStream)
	want := []string{}
	for i := range 3 {
		if b, err := c.Next(context.Background(), byBeaconID); status.Code(err) != codes.Unknown {
			t.Fatalf("Next() %d = %v, %v, want a codes.Unknown error", i, b, err)
		}
		want = append(want, "a", "b", "c")
		if got := opened(); !slices.Equal(got, want) {
			t.Errorf("after Next() %d, streams opened on %v, want %v", i, got, want)
		}
		if got := penalized(fb); len(got) != 0 {
			t.Errorf("after Next() %d, penalized nodes = %v, want none", i, got)
		}
		ctx, served := TrackNode(context.Background())
		if _, err := c.pc.ChainInfo(ctx, &proto.ChainInfoRequest{}); err != nil || served() != "a" {
			t.Errorf("after Next() %d, ChainInfo() = %v on %q, want nil on a", i, err, served())
		}
	}
}

// waitReady waits until fb has n READY nodes.
func waitReady(t *testing.T, fb *fallbackBalancer, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fb.mu.Lock()
		ready := len(rank(slices.Collect(maps.Values(fb.nodes)), fb.now()))
		fb.mu.Unlock()
		if ready == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("balancer has fewer than %d READY nodes", n)
}
