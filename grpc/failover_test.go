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
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// streamNode is a drand node whose PublicRandStream fails with fail, or sends one beacon when fail is nil. Its unary
// calls wait for wait, or until the caller leaves, then fail with err, or answer for round 42 when err is nil.
type streamNode struct {
	proto.UnimplementedPublicServer
	name   string
	fail   func(proto.Public_PublicRandStreamServer) error
	err    error
	wait   time.Duration
	opened func(name string)
	// calls counts the unary calls the node received.
	calls atomic.Int32
}

func (n *streamNode) unary(ctx context.Context) error {
	n.calls.Add(1)
	if n.wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(n.wait):
		}
	}
	return n.err
}

func (n *streamNode) PublicRand(ctx context.Context, _ *proto.PublicRandRequest) (*proto.PublicRandResponse, error) {
	if err := n.unary(ctx); err != nil {
		return nil, err
	}
	return &proto.PublicRandResponse{Round: 42}, nil
}

func (n *streamNode) ChainInfo(ctx context.Context, _ *proto.ChainInfoRequest) (*proto.ChainInfoPacket, error) {
	if err := n.unary(ctx); err != nil {
		return nil, err
	}
	return &proto.ChainInfoPacket{
		Period:   fakePeriod,
		Hash:     fakeChainHash,
		Metadata: &proto.Metadata{BeaconID: "default", ChainHash: fakeChainHash},
	}, nil
}

func (n *streamNode) ListBeaconIDs(ctx context.Context, _ *proto.ListBeaconIDsRequest) (*proto.ListBeaconIDsResponse, error) {
	if err := n.unary(ctx); err != nil {
		return nil, err
	}
	return &proto.ListBeaconIDsResponse{
		Ids:       []string{"default"},
		Metadatas: []*proto.Metadata{{BeaconID: "default", ChainHash: fakeChainHash}},
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
	nodes := make([]*streamNode, len(failing))
	for i, fail := range failing {
		nodes[i] = &streamNode{fail: fail}
	}
	return serve(t, nodes...)
}

// serve serves nodes, named a, b, c and d in order, through a fallback balancer, like serveNodes.
func serve(t *testing.T, nodes ...*streamNode) (*Client, *fallbackBalancer, func() []string) {
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
	addrs := []string{"a", "b", "c", "d"}[:len(nodes)]
	listeners := map[string]*bufconn.Listener{}
	for i, addr := range addrs {
		nodes[i].name, nodes[i].opened = addr, record
		lis := bufconn.Listen(1 << 16)
		srv := grpc.NewServer()
		proto.RegisterPublicServer(srv, nodes[i])
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		listeners[addr] = lis
	}

	c := newClient(nil, slog.New(slog.DiscardHandler))
	t.Cleanup(c.cancel)
	c.minBackoff, c.maxBackoff, c.nodes = time.Millisecond, 10*time.Millisecond, len(addrs)
	conn, err := grpc.NewClient("fallback:///"+strings.Join(addrs, ","),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"recording_`+fallbackName+`"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(c.retry),
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

	c.pc, c.conn = proto.NewPublicClient(conn), conn
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
			s := &sharedStream{waiters: map[chan nextResult]struct{}{ch: {}}, running: true}

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
		if got := next(fb); got != "a" {
			t.Errorf("after Next() %d, next pick = %s, want a", i, got)
		}
	}
}

// next returns the node fb picks for the next call.
func next(fb *fallbackBalancer) string {
	if c := fb.choose(); c != nil {
		return c.addr
	}
	return ""
}

func TestUnaryRetriesOnAnotherNode(t *testing.T) {
	// drand nodes return plain errors, for a chain they do not serve for instance
	plain := errors.New("unknown chain")
	tests := []struct {
		name      string
		call      func(*Client, context.Context) error
		errs      []error // of the nodes a, b, c
		wantErr   bool
		wantCalls []int32
	}{
		{"ChainInfo", func(c *Client, ctx context.Context) error {
			_, err := c.GetChainInfo(ctx, byBeaconID)
			return err
		}, []error{plain, nil, nil}, false, []int32{1, 1, 0}},
		{"ListBeaconIDs", func(c *Client, ctx context.Context) error {
			_, _, err := c.GetBeaconIds(ctx)
			return err
		}, []error{plain, nil, nil}, false, []int32{1, 1, 0}},
		{"ListBeaconIDs and ChainInfo", func(c *Client, ctx context.Context) error {
			_, err := c.GetChains(ctx)
			return err
		}, []error{plain, nil, nil}, false, []int32{2, 2, 0}},
		{"latest PublicRand", func(c *Client, ctx context.Context) error {
			_, err := c.GetBeacon(ctx, byBeaconID, 0)
			return err
		}, []error{plain, nil, nil}, false, []int32{1, 1, 0}},
		{"PublicRand of a round, after ChainInfo", func(c *Client, ctx context.Context) error {
			_, err := c.GetBeacon(ctx, byBeaconID, 42)
			return err
		}, []error{plain, nil, nil}, false, []int32{2, 2, 0}},
		{"one retry only", func(c *Client, ctx context.Context) error {
			_, err := c.GetChainInfo(ctx, byBeaconID)
			return err
		}, []error{plain, plain, nil}, true, []int32{1, 1, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := make([]*streamNode, len(tt.errs))
			for i, err := range tt.errs {
				nodes[i] = &streamNode{err: err}
			}
			c, fb, _ := serve(t, nodes...)
			for i := range 3 {
				for _, n := range nodes {
					n.calls.Store(0)
				}
				ctx, served := TrackNode(context.Background())
				err := tt.call(c, ctx)
				if gotErr := err != nil; gotErr != tt.wantErr {
					t.Fatalf("call %d = %v, want error %v", i, err, tt.wantErr)
				}
				if !tt.wantErr && served() != "b" {
					t.Errorf("call %d served by %q, want b", i, served())
				}
				calls := make([]int32, len(nodes))
				for j, n := range nodes {
					calls[j] = n.calls.Load()
				}
				if !slices.Equal(calls, tt.wantCalls) {
					t.Errorf("call %d made %v calls to a, b, c, want %v", i, calls, tt.wantCalls)
				}
				if got := penalized(fb); len(got) != 0 {
					t.Errorf("after call %d, penalized nodes = %v, want none", i, got)
				}
				if got := next(fb); got != "a" {
					t.Errorf("after call %d, next pick = %s, want a", i, got)
				}
				// the first call cached the chain info
				c.knownChains.Clear()
			}
		})
	}
}

func TestUnaryAttemptTimeout(t *testing.T) {
	const timeout = time.Second
	// a node waits for the round after its latest one for one period and a second, then gives up
	roundWait := fakePeriod*time.Second + time.Second
	never := errors.New("unreachable")
	// the cache only knows the chain by beacon ID
	chainInfo := func(c *Client, ctx context.Context) error {
		_, err := c.GetChainInfo(ctx, byChainHash)
		return err
	}
	round := func(c *Client, ctx context.Context) error {
		_, err := c.GetBeacon(ctx, byBeaconID, 42)
		return err
	}
	tests := []struct {
		name          string
		call          func(*Client, context.Context) error
		wait          time.Duration // of node a, b answers at once
		err           error         // of node a
		deadline      time.Duration // of the caller, none when zero
		wantCode      codes.Code
		wantElapsed   time.Duration
		wantPenalized []string
		wantCallsB    int32
	}{
		{"hanging ChainInfo", chainInfo, time.Hour, nil, 0, codes.OK, timeout, []string{"a"}, 1},
		{"hanging PublicRand of a round", round, time.Hour, nil, 0, codes.OK, roundWait + timeout, []string{"a"}, 1},
		{"node waiting for the next round", round, roundWait, errors.New("waited too long"), 0, codes.OK, roundWait, nil, 1},
		{"caller deadline first", chainInfo, time.Hour, never, timeout / 2, codes.DeadlineExceeded, timeout / 2, nil, 0},
		{"caller deadline during a round wait", round, time.Hour, never, roundWait, codes.DeadlineExceeded, roundWait, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, b := &streamNode{wait: tt.wait, err: tt.err}, &streamNode{}
				c, fb, _ := serve(t, a, b)
				c.timeout = timeout
				c.knownChains.Store("default", &JsonInfoV2{Period: fakePeriod, Hash: fakeChainHash, BeaconId: "default"})
				ctx := context.Background()
				if tt.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.deadline)
					defer cancel()
				}

				start := time.Now()
				err := tt.call(c, ctx)
				if got := status.Code(err); got != tt.wantCode {
					t.Errorf("call = %v, want code %v", err, tt.wantCode)
				}
				if elapsed := time.Since(start); elapsed != tt.wantElapsed {
					t.Errorf("call took %v, want %v", elapsed, tt.wantElapsed)
				}
				if got := penalized(fb); !slices.Equal(got, tt.wantPenalized) {
					t.Errorf("penalized nodes = %v, want %v", got, tt.wantPenalized)
				}
				if got := b.calls.Load(); got != tt.wantCallsB {
					t.Errorf("calls to b = %d, want %d", got, tt.wantCallsB)
				}
			})
		})
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
