package grpc

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
)

type fakeSubConn struct{ balancer.SubConn }

// testBalancer is a fallbackBalancer without a base balancer, on a fake clock.
type testBalancer struct {
	*fallbackBalancer
	clock time.Time
	scs   map[string]balancer.SubConn
}

// newTestBalancer returns a balancer whose nodes are addrs, in configured order, all READY.
func newTestBalancer(addrs ...string) *testBalancer {
	tb := &testBalancer{
		fallbackBalancer: newFallbackBalancer(time.Minute),
		clock:            time.Unix(1_700_000_000, 0),
		scs:              make(map[string]balancer.SubConn),
	}
	tb.now = func() time.Time { return tb.clock }
	tb.setAddresses(resolved(addrs...))
	for _, addr := range addrs {
		tb.scs[addr] = &fakeSubConn{}
	}
	tb.ready(addrs...)
	return tb
}

func resolved(addrs ...string) []resolver.Address {
	res := make([]resolver.Address, len(addrs))
	for i, addr := range addrs {
		res[i] = resolver.Address{Addr: addr, Attributes: attributes.New("order", i)}
	}
	return res
}

// ready makes the base balancer report addrs as the READY nodes.
func (tb *testBalancer) ready(addrs ...string) {
	info := base.PickerBuildInfo{ReadySCs: make(map[balancer.SubConn]base.SubConnInfo)}
	for _, addr := range addrs {
		info.ReadySCs[tb.scs[addr]] = base.SubConnInfo{Address: resolver.Address{Addr: addr}}
	}
	tb.Build(info)
}

// pick returns the address picked for ctx and the function that reports how the call ended.
func (tb *testBalancer) pick(t *testing.T, ctx context.Context) (string, func(error)) {
	t.Helper()
	res, err := (&picker{fb: tb.fallbackBalancer}).Pick(balancer.PickInfo{Ctx: ctx})
	if err != nil {
		t.Fatalf("Pick() = %v", err)
	}
	addr := res.Metadata["target"][0]
	if res.SubConn != tb.scs[addr] {
		t.Fatalf("Pick() = SubConn %p, target %s, want SubConn %p", res.SubConn, addr, tb.scs[addr])
	}
	return addr, func(err error) { res.Done(balancer.DoneInfo{Err: err}) }
}

func TestRank(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ready := &fakeSubConn{}
	n := func(addr string, order int, sc balancer.SubConn, failedAgo, penaltyLeft time.Duration) *node {
		if penaltyLeft == 0 {
			return &node{addr: addr, order: order, sc: sc}
		}
		return &node{addr: addr, order: order, sc: sc, failedAt: now.Add(-failedAgo), penalizedUntil: now.Add(penaltyLeft)}
	}
	tests := []struct {
		name  string
		nodes []*node
		want  []string
	}{
		{"unpenalized by configured order", []*node{
			n("c", 2, ready, 0, 0), n("a", 0, ready, 0, 0), n("b", 1, ready, 0, 0),
		}, []string{"a", "b", "c"}},
		{"penalized behind unpenalized", []*node{
			n("a", 0, ready, time.Second, time.Minute), n("b", 1, ready, 0, 0), n("c", 2, ready, 0, 0),
		}, []string{"b", "c", "a"}},
		{"all penalized by oldest failure", []*node{
			n("a", 0, ready, time.Second, time.Minute), n("b", 1, ready, 3*time.Second, time.Minute),
			n("c", 2, ready, 2*time.Second, time.Minute),
		}, []string{"b", "c", "a"}},
		{"penalized by failure, not by end of penalty", []*node{
			n("a", 0, ready, 2*time.Second, time.Hour), n("b", 1, ready, time.Second, time.Minute),
		}, []string{"a", "b"}},
		{"same failure by configured order", []*node{
			n("b", 1, ready, time.Second, time.Minute), n("a", 0, ready, time.Second, time.Minute),
		}, []string{"a", "b"}},
		{"not READY excluded", []*node{
			n("a", 0, ready, 0, 0), n("b", 1, nil, 0, 0), n("c", 2, ready, time.Second, time.Minute),
		}, []string{"a", "c"}},
		{"penalty ending in a nanosecond still counts", []*node{
			n("a", 0, ready, time.Minute, time.Nanosecond), n("b", 1, ready, 0, 0),
		}, []string{"b", "a"}},
		{"expired penalty is over", []*node{
			n("a", 0, ready, time.Minute, -time.Second), n("b", 1, ready, 0, 0),
		}, []string{"a", "b"}},
		{"late READY node before penalized ones", []*node{
			n("a", 0, ready, 2*time.Second, time.Minute), n("b", 1, ready, time.Second, time.Minute),
			n("c", 2, ready, 0, 0),
		}, []string{"c", "a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, n := range rank(tt.nodes, now) {
				got = append(got, n.addr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("rank(...) = %v, want %v", got, tt.want)
			}
		})
	}

	expired := n("a", 0, ready, time.Minute, time.Second)
	if got := rank([]*node{n("b", 1, ready, 0, 0), expired}, expired.penalizedUntil); got[0] != expired {
		t.Errorf("rank(...) at the end of a's penalty = %s first, want a", got[0].addr)
	}
}

func TestPickerFailover(t *testing.T) {
	ctx := context.Background()
	stream := context.WithValue(ctx, streamPenaltyKey{}, time.Minute)
	long := context.WithValue(ctx, streamPenaltyKey{}, time.Hour)
	stalled, stall := context.WithCancelCause(stream)
	stall(errStreamStalled)
	cancelled, cancel := context.WithCancel(stream)
	cancel()
	excused, stallExcused := context.WithCancelCause(context.WithValue(stream, excusedStallKey{}, true))
	stallExcused(errStreamStalled)
	avoidA, avoidB := AvoidNode(ctx, "a"), AvoidNode(ctx, "b")
	unavailable := status.Error(codes.Unavailable, "node down")
	// drand nodes return plain errors, which reach the client as codes.Unknown
	unknownRound := status.Error(codes.Unknown, "can't retrieve beacon 7")

	type pick struct {
		ready   []string // when set, the READY nodes before the pick
		advance time.Duration
		ctx     context.Context
		err     error // how the call ends
		want    string
	}
	tests := []struct {
		name  string
		picks []pick
	}{
		{"node error moves to the next node", []pick{{nil, 0, ctx, unavailable, "a"}, {nil, 0, ctx, nil, "b"}}},
		{"stream error moves to the next node", []pick{{nil, 0, stream, unknownRound, "a"}, {nil, 0, ctx, nil, "b"}}},
		{"stall moves to the next node", []pick{
			{nil, 0, stalled, status.Error(codes.Canceled, "stalled"), "a"}, {nil, 0, ctx, nil, "b"},
		}},
		{"caller cancel keeps the node", []pick{
			{nil, 0, cancelled, status.Error(codes.Canceled, "gone"), "a"}, {nil, 0, ctx, nil, "a"},
		}},
		{"unknown round keeps the node", []pick{{nil, 0, ctx, unknownRound, "a"}, {nil, 0, ctx, nil, "a"}}},
		{"not found keeps the node", []pick{
			{nil, 0, ctx, status.Error(codes.NotFound, "no such chain"), "a"}, {nil, 0, ctx, nil, "a"},
		}},
		{"invalid argument keeps the node", []pick{
			{nil, 0, ctx, status.Error(codes.InvalidArgument, "bad round"), "a"}, {nil, 0, ctx, nil, "a"},
		}},
		{"expired penalty returns to the first node", []pick{
			{nil, 0, ctx, unavailable, "a"}, {nil, time.Second, ctx, nil, "b"}, {nil, 59 * time.Second, ctx, nil, "a"},
		}},
		{"longer penalty wins", []pick{
			{[]string{"a", "b"}, 0, long, unavailable, "a"}, {nil, time.Second, ctx, unavailable, "b"},
			{nil, time.Second, ctx, unavailable, "a"}, {nil, 2 * time.Minute, ctx, nil, "b"},
		}},
		{"excused stall keeps the node", []pick{
			{nil, 0, excused, status.Error(codes.Canceled, "stalled"), "a"}, {nil, 0, ctx, nil, "a"},
		}},
		{"avoid picks the next node for that call only", []pick{{nil, 0, avoidA, nil, "b"}, {nil, 0, ctx, nil, "a"}}},
		{"failed avoiding call penalizes the node it used", []pick{
			{nil, 0, avoidA, unavailable, "b"}, {nil, 0, avoidA, nil, "c"}, {nil, 0, ctx, nil, "a"},
		}},
		{"avoid with every other node penalized picks the best other", []pick{
			{nil, 0, ctx, unavailable, "a"}, {nil, time.Second, ctx, unavailable, "b"},
			{nil, time.Second, ctx, unavailable, "c"}, {nil, 0, ctx, nil, "a"}, {nil, 0, avoidA, nil, "b"},
		}},
		{"avoid of a penalized node keeps its penalty", []pick{
			{nil, 0, ctx, unavailable, "a"}, {nil, 30 * time.Second, avoidA, nil, "b"}, {nil, 30 * time.Second, ctx, nil, "a"},
		}},
		{"avoid of another node keeps the first node", []pick{{nil, 0, avoidB, nil, "a"}, {nil, 0, ctx, nil, "a"}}},
		{"avoid of an unknown node keeps the first node", []pick{
			{nil, 0, AvoidNode(ctx, ""), nil, "a"}, {nil, 0, AvoidNode(ctx, "x"), nil, "a"},
		}},
		{"avoid with one READY node keeps it", []pick{{[]string{"a"}, 0, avoidA, nil, "a"}, {nil, 0, ctx, nil, "a"}}},
		{"late READY node before penalized ones", []pick{
			{[]string{"a"}, 0, ctx, unavailable, "a"}, {[]string{"a", "b"}, time.Second, ctx, nil, "b"},
			{[]string{"a", "b", "c"}, time.Second, ctx, unavailable, "b"}, {nil, time.Second, ctx, nil, "c"},
		}},
		{"penalty survives a READY flap", []pick{
			{nil, 0, ctx, unavailable, "a"}, {[]string{"b", "c"}, time.Second, ctx, nil, "b"},
			{[]string{"a", "b", "c"}, time.Second, ctx, nil, "b"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := newTestBalancer("a", "b", "c")
			for i, pk := range tt.picks {
				if pk.ready != nil {
					tb.ready(pk.ready...)
				}
				tb.clock = tb.clock.Add(pk.advance)
				got, done := tb.pick(t, pk.ctx)
				if got != pk.want {
					t.Fatalf("Pick %d = %s, want %s", i, got, pk.want)
				}
				done(pk.err)
			}
		})
	}
}

func TestPickerStaysOnFirstNode(t *testing.T) {
	tb := newTestBalancer("a", "b", "c")
	for i := range 1000 {
		tb.clock = tb.clock.Add(time.Second)
		got, done := tb.pick(t, context.Background())
		if got != "a" {
			t.Fatalf("Pick %d after successes = %s, want a", i, got)
		}
		done(nil)
	}
}

func TestPickerWalksEveryNode(t *testing.T) {
	addrs := []string{"a", "b", "c", "d"}
	unavailable := status.Error(codes.Unavailable, "node down")
	for n := 3; n <= len(addrs); n++ {
		tb := newTestBalancer(addrs[:n]...)
		for i, want := range append(addrs[:n:n], "a") {
			tb.clock = tb.clock.Add(time.Millisecond)
			got, done := tb.pick(t, context.Background())
			if got != want {
				t.Fatalf("%d nodes: Pick %d = %s, want %s", n, i, got, want)
			}
			done(unavailable)
		}
	}
}

func TestStreamPenaltyCoversEveryNode(t *testing.T) {
	addrs := []string{"a", "b", "c", "d"}
	backoffs := map[string]func(c *Client, failures uint) time.Duration{
		"longest backoff": func(c *Client, _ uint) time.Duration { return c.maxBackoff },
		"actual backoff":  (*Client).backoff,
	}
	for name, backoff := range backoffs {
		for _, period := range []time.Duration{time.Second, 3 * time.Second, 30 * time.Second} {
			for n := 2; n <= len(addrs); n++ {
				c := newClient(nil, nil)
				c.nodes = n
				stall := stallPeriods * period
				stream := context.WithValue(context.Background(), streamPenaltyKey{}, c.streamPenalty(stall))
				stalled, cancel := context.WithCancelCause(stream)
				cancel(errStreamStalled)
				tb := newTestBalancer(addrs[:n]...)
				for i, want := range append(addrs[:n:n], "a") {
					got, done := tb.pick(t, stalled)
					if got != want {
						t.Fatalf("%s, period %v, %d nodes: Pick %d = %s, want %s", name, period, n, i, got, want)
					}
					tb.clock = tb.clock.Add(stall)
					done(status.Error(codes.Canceled, "stalled"))
					tb.clock = tb.clock.Add(backoff(c, uint(i+1)))
				}
				c.cancel()
			}
		}
	}
}

func TestPickerNoReadyNode(t *testing.T) {
	tb := newTestBalancer("a", "b")
	tb.ready()
	p := &picker{fb: tb.fallbackBalancer}
	if _, err := p.Pick(balancer.PickInfo{Ctx: context.Background()}); !errors.Is(err, balancer.ErrNoSubConnAvailable) {
		t.Errorf("Pick() without READY nodes = %v, want %v", err, balancer.ErrNoSubConnAvailable)
	}

	tb.ready("b")
	if got, _ := tb.pick(t, context.Background()); got != "b" {
		t.Errorf("Pick() with b READY = %s, want b", got)
	}

	failureWatcher{fakeClientConn{}, tb.fallbackBalancer}.UpdateState(balancer.State{
		ConnectivityState: connectivity.TransientFailure,
	})
	if c := tb.current.Load(); c != nil {
		t.Errorf("choice after TRANSIENT_FAILURE = %s, want none", c.addr)
	}
}

type fakeClientConn struct{ balancer.ClientConn }

func (fakeClientConn) UpdateState(balancer.State) {}

func TestSetAddresses(t *testing.T) {
	tb := newTestBalancer("a", "b")
	unordered := resolver.Address{Addr: "x"}
	got := tb.setAddresses(append(resolved("a", "b", "a"), unordered))
	if want := resolved("a", "b"); !slices.EqualFunc(got, want, resolver.Address.Equal) {
		t.Errorf("setAddresses(a, b, a, x) = %v, want %v", got, want)
	}

	_, done := tb.pick(t, context.Background())
	done(status.Error(codes.Unavailable, "node down"))
	tb.setAddresses(resolved("c", "b", "a"))
	tb.scs["c"] = &fakeSubConn{}
	tb.ready("a", "b", "c")
	if got, _ := tb.pick(t, context.Background()); got != "c" {
		t.Errorf("Pick() after adding c first = %s, want c", got)
	}
	if order := tb.nodes["a"].order; order != 2 {
		t.Errorf("order of a after the reorder = %d, want 2", order)
	}
	if !tb.clock.Before(tb.nodes["a"].penalizedUntil) {
		t.Error("reorder dropped the penalty of a")
	}

	got = tb.setAddresses([]resolver.Address{{Addr: "b", Attributes: attributes.New("order", 3)}, resolved("b")[0]})
	if want := resolved("b"); !slices.EqualFunc(got, want, resolver.Address.Equal) {
		t.Errorf("setAddresses(b at 3, b at 0) = %v, want %v", got, want)
	}
	if _, ok := tb.nodes["a"]; ok {
		t.Error("setAddresses(b) kept node a")
	}
	if got, _ := tb.pick(t, context.Background()); got != "b" {
		t.Errorf("Pick() after removing a and c = %s, want b", got)
	}
}

func TestZeroAddresses(t *testing.T) {
	fb := fallbackBB{time.Minute}.Build(fakeClientConn{}, balancer.BuildOptions{}).(*fallbackBalancer)
	err := fb.UpdateClientConnState(balancer.ClientConnState{})
	if !errors.Is(err, balancer.ErrBadResolverState) {
		t.Errorf("UpdateClientConnState(no addresses) = %v, want %v", err, balancer.ErrBadResolverState)
	}
	if c := fb.current.Load(); c != nil {
		t.Errorf("choice without addresses = %s, want none", c.addr)
	}
}

func TestTrackNode(t *testing.T) {
	tb := newTestBalancer("a", "b", "c")
	ctx, last := TrackNode(context.Background())
	if got := last(); got != "" {
		t.Errorf("TrackNode() before any pick = %q, want \"\"", got)
	}
	for _, want := range []string{"a", "b"} {
		_, done := tb.pick(t, ctx)
		done(status.Error(codes.Unavailable, "node down"))
		if got := last(); got != want {
			t.Errorf("TrackNode() after picking %s = %q, want %q", want, got, want)
		}
	}
	// c is now first
	tb.pick(t, AvoidNode(ctx, "c"))
	if got := last(); got != "a" {
		t.Errorf("TrackNode() after avoiding c = %q, want \"a\"", got)
	}
}

func TestAvoidNodeKeepsState(t *testing.T) {
	tb := newTestBalancer("a", "b", "c")
	for range 100 {
		tb.clock = tb.clock.Add(time.Second)
		if got, done := tb.pick(t, AvoidNode(context.Background(), "a")); got != "b" {
			t.Fatalf("Pick(AvoidNode(a)) = %s, want b", got)
		} else {
			done(nil)
		}
	}
	for addr, n := range tb.nodes {
		if !n.penalizedUntil.IsZero() {
			t.Errorf("node %s penalized until %v after avoids, want no penalty", addr, n.penalizedUntil)
		}
	}
	if got, _ := tb.pick(t, context.Background()); got != "a" {
		t.Errorf("Pick() after avoids of a = %s, want a", got)
	}
}

// TestBalancerConcurrency lets the race detector check picks, penalties, readiness and address changes together.
func TestBalancerConcurrency(t *testing.T) {
	tb := newTestBalancer("a", "b", "c")
	tb.now = time.Now
	p := &picker{fb: tb.fallbackBalancer}
	ctxs := []context.Context{
		context.Background(),
		AvoidNode(context.Background(), "a"),
		context.WithValue(context.Background(), streamPenaltyKey{}, time.Millisecond),
	}
	readySets := [][]string{{"a", "b", "c"}, {"b"}, {}, {"a", "c"}}
	addrSets := [][]resolver.Address{resolved("a", "b", "c"), resolved("c", "a"), resolved("b")}
	const iterations = 500

	var wg sync.WaitGroup
	for _, ctx := range ctxs {
		wg.Go(func() {
			for range iterations {
				res, err := p.Pick(balancer.PickInfo{Ctx: ctx})
				if err != nil {
					continue
				}
				if addr := res.Metadata["target"][0]; res.SubConn != tb.scs[addr] {
					t.Errorf("Pick() = SubConn %p, target %s, want SubConn %p", res.SubConn, addr, tb.scs[addr])
				}
				res.Done(balancer.DoneInfo{Err: status.Error(codes.Unavailable, "node down")})
			}
		})
	}
	wg.Go(func() {
		for i := range iterations {
			tb.ready(readySets[i%len(readySets)]...)
		}
	})
	wg.Go(func() {
		for i := range iterations {
			tb.setAddresses(addrSets[i%len(addrSets)])
		}
	})
	wg.Wait()
}

func TestNodeFault(t *testing.T) {
	ctx := context.Background()
	internal := status.Error(codes.Internal, "grpc: error while marshaling")
	tests := []struct {
		name   string
		di     balancer.DoneInfo
		stream bool
		want   bool
	}{
		{"success", balancer.DoneInfo{BytesSent: true, BytesReceived: true}, false, false},
		{"internal from the node", balancer.DoneInfo{Err: internal, BytesSent: true, BytesReceived: true}, false, true},
		{"internal from the client", balancer.DoneInfo{Err: internal, BytesSent: true}, false, false},
		{"internal from the client on a stream", balancer.DoneInfo{Err: internal, BytesSent: true}, true, true},
		{"unavailable before any byte", balancer.DoneInfo{Err: status.Error(codes.Unavailable, "node down")}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeFault(ctx, tt.di, tt.stream); got != tt.want {
				t.Errorf("nodeFault(%+v, stream %v) = %v, want %v", tt.di, tt.stream, got, tt.want)
			}
		})
	}
}
