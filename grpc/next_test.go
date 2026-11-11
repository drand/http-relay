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

const fakePeriod = 3

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
		Period:   fakePeriod,
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
		// failures is how many streams in a row break while the callers wait, they call Next again each time
		failures    int
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
			name:        "stream errors then recovery",
			callers:     []*proto.Metadata{byBeaconID, byChainHash, byBeaconID},
			failures:    2,
			wantStreams: 3,
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

					waiting := len(callers) - slices.Index(tt.cancelled, i) - 1
					c.nextMu.Lock()
					for key, s := range c.streams {
						if len(s.waiters) != waiting {
							t.Errorf("stream %s has %d waiters after %d cancellations, want %d", key, len(s.waiters), len(callers)-waiting, waiting)
						}
					}
					c.nextMu.Unlock()
				}

				for n := range tt.failures {
					node.opened()[n].recv <- recvResult{err: errStream}
					for i, m := range callers {
						if m == nil {
							continue
						}
						if res := <-results[i]; !errors.Is(res.err, errStream) {
							t.Errorf("Next(caller %d) on broken stream = %v, %v, want error %v", i, res.beacon, res.err, errStream)
						}
					}
					results, _ = callNext(c, callers)
					// the backoff doubles with each failure, the stream must reopen exactly when it ends
					time.Sleep(c.minBackoff<<n - time.Nanosecond)
					synctest.Wait()
					if got := len(node.opened()); got != n+1 {
						t.Fatalf("opened %d streams before the backoff of failure %d ended, want %d", got, n+1, n+1)
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					if got := len(node.opened()); got != n+2 {
						t.Fatalf("opened %d streams after the backoff of failure %d, want %d", got, n+1, n+2)
					}
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

func TestNextStalledStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := &fakeNode{}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		stall := stallPeriods * fakePeriod * time.Second

		results, _ := callNext(c, []*proto.Metadata{byBeaconID})
		time.Sleep(stall - time.Nanosecond)
		synctest.Wait()
		node.opened()[0].recv <- recvResult{resp: &proto.PublicRandResponse{Round: 42}}
		if res := <-results[0]; res.err != nil {
			t.Fatalf("Next() just before the stall timeout = %v, %v, want a beacon", res.beacon, res.err)
		}

		// the beacon restarted the timeout
		results, _ = callNext(c, []*proto.Metadata{byBeaconID})
		time.Sleep(stall - time.Nanosecond)
		synctest.Wait()
		if got := len(node.opened()); got != 1 {
			t.Fatalf("opened %d streams before the stall timeout, want 1", got)
		}

		// each stall reopens the stream
		const wantStreams = 3
		time.Sleep(time.Nanosecond + c.minBackoff)
		synctest.Wait()
		time.Sleep(stall + 2*c.minBackoff)
		synctest.Wait()
		streams := node.opened()
		if len(streams) != wantStreams {
			t.Fatalf("opened %d streams after two stalls, want %d", len(streams), wantStreams)
		}

		streams[2].recv <- recvResult{resp: &proto.PublicRandResponse{Round: 43}}
		if res := <-results[0]; res.err != nil || res.beacon.Round != 43 {
			t.Errorf("Next() across stalls = %v, %v, want round 43", res.beacon, res.err)
		}

		// without waiters, a stall closes the stream for good
		time.Sleep(stall)
		synctest.Wait()
		time.Sleep(c.maxBackoff)
		synctest.Wait()
		if got := len(node.opened()); got != wantStreams {
			t.Errorf("opened %d streams after a stall without waiters, want %d", got, wantStreams)
		}
		c.nextMu.Lock()
		for key, s := range c.streams {
			if s.running {
				t.Errorf("stream %s still running after a stall without waiters", key)
			}
		}
		c.nextMu.Unlock()
	})
}

func TestNextStreamErrorTriesEveryNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := &fakeNode{}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 3
		errStream := errors.New("beacon has not started on this node yet")

		results, _ := callNext(c, []*proto.Metadata{byBeaconID})
		for n := range c.nodes {
			synctest.Wait()
			select {
			case res := <-results[0]:
				t.Fatalf("Next() after %d stream errors = %v, %v, want it to keep waiting", n, res.beacon, res.err)
			default:
			}
			node.opened()[n].recv <- recvResult{err: errStream}
			time.Sleep(c.maxBackoff)
		}
		if res := <-results[0]; !errors.Is(res.err, errStream) {
			t.Errorf("Next() after an error on every node = %v, %v, want error %v", res.beacon, res.err, errStream)
		}
		if got, want := len(node.opened()), c.nodes; got != want {
			t.Errorf("opened %d streams, want %d", got, want)
		}
	})
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
