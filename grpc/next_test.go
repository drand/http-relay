package grpc

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"google.golang.org/grpc"
)

var (
	fakeChainHash = []byte{0xde, 0xad, 0xbe, 0xef}
	byBeaconID    = &proto.Metadata{BeaconID: "default"}
	byChainHash   = &proto.Metadata{ChainHash: fakeChainHash}
)

type recvResult struct {
	resp *proto.PublicRandResponse
	err  error
}

type fakeStream struct {
	grpc.ClientStream
	ctx  context.Context
	recv chan recvResult
}

func (s *fakeStream) Recv() (*proto.PublicRandResponse, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case r := <-s.recv:
		return r.resp, r.err
	}
}

// fakeNode serves a single chain, known both as beacon ID "default" and as fakeChainHash.
type fakeNode struct {
	proto.PublicClient
	mu      sync.Mutex
	streams []*fakeStream
}

func (n *fakeNode) ChainInfo(_ context.Context, in *proto.ChainInfoRequest, _ ...grpc.CallOption) (*proto.ChainInfoPacket, error) {
	m := in.GetMetadata()
	if m.GetBeaconID() != "default" && string(m.GetChainHash()) != string(fakeChainHash) {
		return nil, errors.New("unknown chain")
	}
	return &proto.ChainInfoPacket{
		Period:   3,
		Metadata: &proto.Metadata{BeaconID: "default", ChainHash: fakeChainHash},
	}, nil
}

func (n *fakeNode) PublicRandStream(ctx context.Context, _ *proto.PublicRandRequest, _ ...grpc.CallOption) (proto.Public_PublicRandStreamClient, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := &fakeStream{ctx: ctx, recv: make(chan recvResult)}
	n.streams = append(n.streams, s)
	return s, nil
}

func (n *fakeNode) opened() []*fakeStream {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.streams)
}

// callNext starts one Next call per metadata, skipping the nil ones.
func callNext(c *Client, callers []*proto.Metadata) ([]chan nextResult, []context.CancelFunc) {
	results := make([]chan nextResult, len(callers))
	cancels := make([]context.CancelFunc, len(callers))
	for i, m := range callers {
		if m == nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		results[i], cancels[i] = make(chan nextResult, 1), cancel
		go func() {
			b, err := c.Next(ctx, m)
			results[i] <- nextResult{b, err}
		}()
	}
	return results, cancels
}

func TestNextSharesStream(t *testing.T) {
	errStream := errors.New("stream broke")

	tests := []struct {
		name    string
		callers []*proto.Metadata
		// cancelled lists the callers that give up before a beacon arrives
		cancelled []int
		// failFirst breaks the first stream while the callers wait, they then call Next again
		failFirst   bool
		wantStreams int
	}{
		{
			name:        "single caller",
			callers:     []*proto.Metadata{byBeaconID},
			wantStreams: 1,
		},
		{
			name:        "concurrent callers",
			callers:     []*proto.Metadata{byBeaconID, byBeaconID, byBeaconID, byBeaconID, byBeaconID},
			wantStreams: 1,
		},
		{
			name:        "beacon ID and chain hash of the same chain",
			callers:     []*proto.Metadata{byBeaconID, byChainHash, byBeaconID, byChainHash},
			wantStreams: 1,
		},
		{
			name:        "cancelled callers",
			callers:     []*proto.Metadata{byBeaconID, byChainHash, byBeaconID, byChainHash},
			cancelled:   []int{0, 3},
			wantStreams: 1,
		},
		{
			name:        "stream error then recovery",
			callers:     []*proto.Metadata{byBeaconID, byChainHash, byBeaconID},
			failFirst:   true,
			wantStreams: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				node := &fakeNode{}
				c := newClient(node, slog.New(slog.DiscardHandler))
				defer c.cancel()

				callers := slices.Clone(tt.callers)
				results, cancels := callNext(c, callers)
				synctest.Wait()

				for _, i := range tt.cancelled {
					cancels[i]()
					if res := <-results[i]; !errors.Is(res.err, context.Canceled) {
						t.Errorf("cancelled Next(caller %d) error = %v, want %v", i, res.err, context.Canceled)
					}
					callers[i] = nil
				}

				if tt.failFirst {
					node.opened()[0].recv <- recvResult{err: errStream}
					for i, m := range callers {
						if m == nil {
							continue
						}
						if res := <-results[i]; !errors.Is(res.err, errStream) {
							t.Errorf("Next(caller %d) on broken stream = %v, %v, want error %v", i, res.beacon, res.err, errStream)
						}
					}
					results, _ = callNext(c, callers)
					// the bubble's fake clock makes this instant
					time.Sleep(c.maxBackoff)
					synctest.Wait()
				}

				streams := node.opened()
				if len(streams) != tt.wantStreams {
					t.Fatalf("opened %d streams, want %d", len(streams), tt.wantStreams)
				}
				// the second wave proves that the stream outlives a delivery
				for round := uint64(42); round < 44; round++ {
					streams[len(streams)-1].recv <- recvResult{resp: &proto.PublicRandResponse{Round: round, Signature: []byte{1, 2, 3}}}

					for i, m := range callers {
						if m == nil {
							continue
						}
						res := <-results[i]
						if res.err != nil {
							t.Errorf("Next(caller %d) error = %v, want nil", i, res.err)
							continue
						}
						if res.beacon.Round != round {
							t.Errorf("Next(caller %d) round = %d, want %d", i, res.beacon.Round, round)
						}
						// handlers do this concurrently, the race detector fails the test if callers share a beacon
						go res.beacon.SetRandomness()
					}

					results, _ = callNext(c, callers)
					synctest.Wait()
				}
				// no beacon comes for the last wave, closing the client releases it
				c.cancel()

				synctest.Wait()
				if got := len(node.opened()); got != tt.wantStreams {
					t.Errorf("opened %d streams, want %d", got, tt.wantStreams)
				}
				c.nextMu.Lock()
				for key, s := range c.streams {
					if len(s.waiters) != 0 {
						t.Errorf("stream %s has %d waiters left, want 0", key, len(s.waiters))
					}
				}
				c.nextMu.Unlock()
				for i, m := range callers {
					if m != nil {
						<-results[i]
					}
				}
			})
		})
	}
}

func TestNextUnknownChain(t *testing.T) {
	c := newClient(&fakeNode{}, slog.New(slog.DiscardHandler))
	defer c.cancel()

	if b, err := c.Next(context.Background(), &proto.Metadata{BeaconID: "unknown"}); err == nil {
		t.Errorf("Next(unknown chain) = %v, nil, want an error", b)
	}
	if len(c.streams) != 0 {
		t.Errorf("Next(unknown chain) left %d streams, want 0", len(c.streams))
	}
}

func TestNextClosedClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newClient(&fakeNode{}, slog.New(slog.DiscardHandler))

		results, _ := callNext(c, []*proto.Metadata{byBeaconID})
		synctest.Wait()
		c.cancel()
		if res := <-results[0]; !errors.Is(res.err, errClientClosed) {
			t.Errorf("Next() during close = %v, %v, want error %v", res.beacon, res.err, errClientClosed)
		}

		if b, err := c.Next(context.Background(), byBeaconID); !errors.Is(err, errClientClosed) {
			t.Errorf("Next() after close = %v, %v, want error %v", b, err, errClientClosed)
		}
	})
}
