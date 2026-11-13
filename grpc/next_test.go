package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/status"
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

// fakeNode serves a single chain, known both as beacon ID "default" and as fakeChainHash. Its streams fail with fail
// when it is set.
type fakeNode struct {
	proto.PublicClient
	fail    error
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
	s := &fakeStream{ctx: ctx, recv: make(chan recvResult, 1)}
	if n.fail != nil {
		s.recv <- recvResult{err: n.fail}
	}
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

func TestNextStreamErrorOnEveryNodeWaitsOneBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tb := newTestBalancer("a", "b", "c")
		tb.now = time.Now
		errStream := errors.New("beacon has not started on this node yet")
		node := &balancedNode{fakeNode: fakeNode{fail: errStream}, tb: tb}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 3

		// the backoff reaches maxBackoff after the first calls
		for i := range 5 {
			before, start := len(node.pickedNodes()), time.Now()
			if b, err := c.Next(context.Background(), byBeaconID); !errors.Is(err, errStream) {
				t.Fatalf("Next() %d = %v, %v, want error %v", i, b, err, errStream)
			}
			// one backoff before the first node, the minimum between nodes
			if elapsed, limit := time.Since(start), c.maxBackoff+2*c.minBackoff; elapsed > limit {
				t.Errorf("Next() %d returned after %v, want at most %v", i, elapsed, limit)
			}
			if got, want := node.pickedNodes()[before:], []string{"a", "b", "c"}; !slices.Equal(got, want) {
				t.Errorf("Next() %d opened streams on %v, want %v", i, got, want)
			}
		}
		checkNoPenalty(t, tb)
	})
}

// TestNextFailsWithoutPickedNode checks that an error with no node picked, as the balancer reports when every node is
// down, fails the callers without retrying.
func TestNextFailsWithoutPickedNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tb := newTestBalancer("a", "b", "c")
		tb.ready()
		node := &balancedNode{tb: tb}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 3

		if b, err := c.Next(context.Background(), byBeaconID); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
			t.Errorf("Next() without READY node = %v, %v, want error %v", b, err, balancer.ErrNoSubConnAvailable)
		}
		if got := len(node.pickedNodes()); got != 0 {
			t.Errorf("Next() without READY node opened %d streams, want 0", got)
		}
	})
}

// TestNextSlowPick checks that the stall window starts once the stream is open, and that a stall while waiting for a
// node blames none.
func TestNextSlowPick(t *testing.T) {
	stall := stallPeriods * fakePeriod * time.Second
	tests := []struct {
		name       string
		delay      time.Duration
		wantPicked []string
	}{
		{"node READY just before the stall", stall - time.Second, []string{"a"}},
		{"no node READY until the stall", stall + time.Second, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tb := newTestBalancer("a", "b", "c")
				tb.now = time.Now
				node := &balancedNode{tb: tb, openDelay: tt.delay}
				c := newClient(node, slog.New(slog.DiscardHandler))
				defer c.cancel()
				c.nodes = 3

				results, _ := callNext(c, []*proto.Metadata{byBeaconID})
				// a beacon after the stall measured from the call, within the stall measured from the open
				time.Sleep(stall + stall/2)
				synctest.Wait()
				if got := node.pickedNodes(); !slices.Equal(got, tt.wantPicked) {
					t.Fatalf("stream opened on %v, want %v", got, tt.wantPicked)
				}
				c.nextMu.Lock()
				for key, s := range c.streams {
					if len(s.failed) != 0 {
						t.Errorf("stream %s failed on %v, want no node", key, s.failed)
					}
				}
				c.nextMu.Unlock()
				if tt.wantPicked != nil {
					node.opened()[0].recv <- recvResult{resp: &proto.PublicRandResponse{Round: 42}}
					if res := <-results[0]; res.err != nil || res.beacon.Round != 42 {
						t.Errorf("Next() = %v, %v, want round 42", res.beacon, res.err)
					}
				}
				c.cancel()
				synctest.Wait()
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

// balancedNode opens its streams on the node the picker of tb chooses, after openDelay as if it waited for a READY
// node, and reports to the picker how they ended.
type balancedNode struct {
	fakeNode
	tb        *testBalancer
	openDelay time.Duration
	picked    []string // guarded by fakeNode.mu
}

func (n *balancedNode) PublicRandStream(ctx context.Context, in *proto.PublicRandRequest, opts ...grpc.CallOption) (proto.Public_PublicRandStreamClient, error) {
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-time.After(n.openDelay):
	}
	res, err := (&picker{fb: n.tb.fallbackBalancer}).Pick(balancer.PickInfo{Ctx: ctx})
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		res.Done(balancer.DoneInfo{Err: status.FromContextError(ctx.Err()).Err()})
	}()
	n.mu.Lock()
	n.picked = append(n.picked, res.Metadata["target"][0])
	n.mu.Unlock()
	return n.fakeNode.PublicRandStream(ctx, in, opts...)
}

func (n *balancedNode) pickedNodes() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.picked)
}

// checkNoPenalty fails t if a node of tb is penalized, or if unary calls left the first node.
func checkNoPenalty(t *testing.T, tb *testBalancer) {
	t.Helper()
	tb.mu.Lock()
	for addr, n := range tb.nodes {
		if !n.penalizedUntil.IsZero() {
			t.Errorf("node %s penalized until %v, want no penalty", addr, n.penalizedUntil)
		}
	}
	tb.mu.Unlock()
	if got, _ := tb.pick(t, context.Background()); got != "a" {
		t.Errorf("Pick() = %s, want a", got)
	}
}

func TestNextHaltedChain(t *testing.T) {
	addrs := []string{"a", "b", "c", "d"}
	for n := 3; n <= len(addrs); n++ {
		t.Run(fmt.Sprintf("%d nodes", n), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tb := newTestBalancer(addrs[:n]...)
				tb.now = time.Now
				node := &balancedNode{tb: tb}
				c := newClient(node, slog.New(slog.DiscardHandler))
				defer c.cancel()
				c.nodes = n
				stall := stallPeriods * fakePeriod * time.Second

				results, _ := callNext(c, []*proto.Metadata{byBeaconID})
				time.Sleep(3 * time.Duration(n) * (stall + c.maxBackoff))
				synctest.Wait()
				select {
				case res := <-results[0]:
					t.Fatalf("Next() on a halted chain = %v, %v, want it to keep waiting", res.beacon, res.err)
				default:
				}
				picked := node.pickedNodes()
				if len(picked) < 2*n {
					t.Fatalf("stream opened on %v, want at least two walks of %v", picked, addrs[:n])
				}
				for i, addr := range picked {
					if want := addrs[i%n]; addr != want {
						t.Fatalf("stream opened on %v, want walks of %v", picked, addrs[:n])
					}
				}
				checkNoPenalty(t, tb)

				// a node that resumes delivers to the callers still waiting
				for streams := node.opened(); streams[len(streams)-1].ctx.Err() != nil; streams = node.opened() {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				streams := node.opened()
				streams[len(streams)-1].recv <- recvResult{resp: &proto.PublicRandResponse{Round: 42}}
				if res := <-results[0]; res.err != nil || res.beacon.Round != 42 {
					t.Fatalf("Next() after the chain resumed = %v, %v, want round 42", res.beacon, res.err)
				}
				checkNoPenalty(t, tb)
			})
		})
	}
}

// TestNextStuckNodeRestarts checks that a node stuck on the chain holds each caller for one stall at most, even when the
// stream stops between them because no caller is left.
func TestNextStuckNodeRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tb := newTestBalancer("a", "b", "c")
		tb.now = time.Now
		node := &balancedNode{tb: tb}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 3
		stall := stallPeriods * fakePeriod * time.Second

		for i := range 3*c.nodes + 1 {
			before := len(node.pickedNodes())
			results, _ := callNext(c, []*proto.Metadata{byBeaconID})
			// a stalls once, then the stream moves on to the healthy node that did not stall last
			time.Sleep(stall + c.maxBackoff)
			synctest.Wait()
			streams := node.opened()
			want := []string{"a", []string{"b", "c"}[i%2]}
			if got := node.pickedNodes()[before:]; !slices.Equal(got, want) {
				t.Fatalf("restart %d: stream opened on %v, want %v", i, got, want)
			}
			streams[len(streams)-1].recv <- recvResult{resp: &proto.PublicRandResponse{Round: uint64(i)}}
			if res := <-results[0]; res.err != nil || res.beacon.Round != uint64(i) {
				t.Fatalf("restart %d: Next() = %v, %v, want round %d", i, res.beacon, res.err, i)
			}
			// without callers, the next stall stops the stream
			time.Sleep(stall + c.maxBackoff)
			synctest.Wait()
			c.nextMu.Lock()
			for key, s := range c.streams {
				if s.running {
					t.Fatalf("restart %d: stream %s still running without callers", i, key)
				}
			}
			c.nextMu.Unlock()
		}
		checkNoPenalty(t, tb)
	})
}

// TestNextCallersLeaveBeforeStall checks that a stuck node is skipped by the next start of the stream even when no
// caller waited for its stall.
func TestNextCallersLeaveBeforeStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tb := newTestBalancer("a", "b", "c")
		tb.now = time.Now
		node := &balancedNode{tb: tb}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 3
		stall := stallPeriods * fakePeriod * time.Second

		results, cancels := callNext(c, []*proto.Metadata{byBeaconID})
		synctest.Wait()
		cancels[0]()
		if res := <-results[0]; !errors.Is(res.err, context.Canceled) {
			t.Fatalf("cancelled Next() = %v, %v, want error %v", res.beacon, res.err, context.Canceled)
		}
		time.Sleep(stall + c.maxBackoff)
		synctest.Wait()
		if got := node.pickedNodes(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("stream opened on %v before the second caller, want [a]", got)
		}

		results, _ = callNext(c, []*proto.Metadata{byBeaconID})
		synctest.Wait()
		if got := node.pickedNodes(); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("stream opened on %v for the second caller, want [a b]", got)
		}
		streams := node.opened()
		streams[len(streams)-1].recv <- recvResult{resp: &proto.PublicRandResponse{Round: 42}}
		if res := <-results[0]; res.err != nil || res.beacon.Round != 42 {
			t.Errorf("Next() after a stall without callers = %v, %v, want round 42", res.beacon, res.err)
		}
		checkNoPenalty(t, tb)
	})
}

// TestNextStuckAfterBreak checks that a stream breaking on the healthy node goes back to it after one stall on the stuck
// node, rather than retrying the stuck one.
func TestNextStuckAfterBreak(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		ready []string
	}{
		{"two nodes", []string{"a", "b"}, []string{"a", "b"}},
		{"third node not READY", []string{"a", "b", "c"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tb := newTestBalancer(tt.addrs...)
				tb.ready(tt.ready...)
				tb.now = time.Now
				node := &balancedNode{tb: tb}
				c := newClient(node, slog.New(slog.DiscardHandler))
				defer c.cancel()
				c.nodes = len(tt.addrs)
				stall := stallPeriods * fakePeriod * time.Second
				latest := func() *fakeStream {
					streams := node.opened()
					return streams[len(streams)-1]
				}

				results, _ := callNext(c, []*proto.Metadata{byBeaconID})
				time.Sleep(stall + c.maxBackoff)
				synctest.Wait()
				latest().recv <- recvResult{resp: &proto.PublicRandResponse{Round: 42}}
				if res := <-results[0]; res.err != nil {
					t.Fatalf("Next() = %v, %v, want round 42", res.beacon, res.err)
				}

				results, _ = callNext(c, []*proto.Metadata{byBeaconID})
				synctest.Wait()
				latest().recv <- recvResult{err: errors.New("stream broke")}
				time.Sleep(stall + c.maxBackoff)
				synctest.Wait()
				if got, want := node.pickedNodes(), []string{"a", "b", "a", "b"}; !slices.Equal(got, want) {
					t.Fatalf("stream opened on %v, want %v", got, want)
				}
				latest().recv <- recvResult{resp: &proto.PublicRandResponse{Round: 43}}
				if res := <-results[0]; res.err != nil || res.beacon.Round != 43 {
					t.Errorf("Next() after the break = %v, %v, want round 43", res.beacon, res.err)
				}
				checkNoPenalty(t, tb)
			})
		})
	}
}

// TestNextHaltErrorsApart checks that stream errors separated by a stall do not add up and fail the callers of a
// halted chain.
func TestNextHaltErrorsApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tb := newTestBalancer("a", "b")
		tb.now = time.Now
		node := &balancedNode{tb: tb}
		c := newClient(node, slog.New(slog.DiscardHandler))
		defer c.cancel()
		c.nodes = 2
		stall := stallPeriods * fakePeriod * time.Second
		latest := func() *fakeStream {
			streams := node.opened()
			return streams[len(streams)-1]
		}

		results, _ := callNext(c, []*proto.Metadata{byBeaconID})
		synctest.Wait()
		latest().recv <- recvResult{err: errors.New("node restarted")}
		// the next node stalls, then the stream reopens
		time.Sleep(stall + c.maxBackoff)
		synctest.Wait()
		latest().recv <- recvResult{err: errors.New("node restarted")}
		time.Sleep(c.maxBackoff)
		synctest.Wait()
		latest().recv <- recvResult{resp: &proto.PublicRandResponse{Round: 7}}
		if res := <-results[0]; res.err != nil || res.beacon.Round != 7 {
			t.Errorf("Next() = %v, %v, want round 7", res.beacon, res.err)
		}
		checkNoPenalty(t, tb)
	})
}
