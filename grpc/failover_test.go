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
	"google.golang.org/grpc/credentials/insecure"
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

func TestStreamReopensOnNextNode(t *testing.T) {
	registerRecording.Do(func() {
		balancer.Register(recordingBuilder{NewFallbackBuilder(FallbackTimeout), builtBalancers})
	})

	stall := func(stream proto.Public_PublicRandStreamServer) error {
		<-stream.Context().Done()
		return nil
	}
	fail := func(proto.Public_PublicRandStreamServer) error {
		return errors.New("beacon has not started on this node yet")
	}
	tests := []struct {
		name    string
		failing []func(proto.Public_PublicRandStreamServer) error
	}{
		{"stall", []func(proto.Public_PublicRandStreamServer) error{stall}},
		{"error", []func(proto.Public_PublicRandStreamServer) error{fail}},
		{"three failing nodes", []func(proto.Public_PublicRandStreamServer) error{stall, fail, stall}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				opened []string
			)
			record := func(name string) {
				mu.Lock()
				defer mu.Unlock()
				opened = append(opened, name)
			}
			addrs := []string{"a", "b", "c", "d"}[:len(tt.failing)+1]
			listeners := map[string]*bufconn.Listener{}
			for i, addr := range addrs {
				node := &streamNode{name: addr, opened: record}
				if i < len(tt.failing) {
					node.fail = tt.failing[i]
				}
				lis := bufconn.Listen(1 << 16)
				srv := grpc.NewServer()
				proto.RegisterPublicServer(srv, node)
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
			// a long maxBackoff only lengthens the stream penalty: the actual backoffs stay a few milliseconds
			c.minBackoff, c.maxBackoff, c.nodes = time.Millisecond, time.Second, len(addrs)
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
			c.cancel()
			<-done

			mu.Lock()
			defer mu.Unlock()
			// only the last node sends beacons, a slow runner may still reopen the stream after it
			if len(opened) < len(addrs) || !slices.Equal(opened[:len(addrs)], addrs) {
				t.Errorf("streams opened on %v, want %v first", opened, addrs)
			}
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
