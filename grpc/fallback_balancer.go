package grpc

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
)

var (
	RequestsCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_client_requests_per_backend",
			Help: "The total number of requests done per backend node",
		},
		[]string{"node"},
	)
)

var fbLog = grpclog.Component("fallbackLB")

const fallbackName = "pick_first_with_fallback"

// NewFallbackBuilder returns a fallback balancer builder configured with the given timeout, meant to be registered.
func NewFallbackBuilder(timeout time.Duration) balancer.Builder {
	fbLog.Info("building balancer", "timeout", timeout)

	return &fallbackBB{
		timeout: timeout,
	}
}

type fallbackBB struct {
	timeout time.Duration
}

func (fallbackBB) Name() string {
	return fallbackName
}

func (f fallbackBB) Build(cc balancer.ClientConn, bOpts balancer.BuildOptions) balancer.Balancer {
	fb := newFallbackBalancer(f.timeout)
	// we delegate the actual SubConn management to the base balancer
	baseBuilder := base.NewBalancerBuilder(fallbackName, fb,
		base.Config{
			//TODO: understand better whether or not we want to rely on the built-in grpc HealthCheck
			HealthCheck: false,
		})
	fb.Balancer = baseBuilder.Build(failureWatcher{cc, fb}, bOpts)
	return fb
}

// fallbackBalancer sends every call to the first configured node that is READY and not penalized. A node that fails a
// call is penalized: it goes behind the others until its penalty ends.
type fallbackBalancer struct {
	// Embeds balancer.Balancer because needs to intercept UpdateClientConnState to learn state.
	balancer.Balancer
	// timeout is how long a node stays behind the others after failing a unary call or being skipped.
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex // guards nodes and the writes to current
	nodes map[string]*node
	// current lets Pick read the choice without the lock. It is nil when no node is READY.
	current atomic.Pointer[choice]
}

func newFallbackBalancer(timeout time.Duration) *fallbackBalancer {
	return &fallbackBalancer{timeout: timeout, now: time.Now, nodes: make(map[string]*node)}
}

// node is a configured address. It outlives its SubConns so that a node keeps its penalty across reconnections.
type node struct {
	addr           string
	order          int
	sc             balancer.SubConn // nil unless READY
	failedAt       time.Time
	penalizedUntil time.Time
}

// choice is an immutable snapshot of the node to pick.
type choice struct {
	sc   balancer.SubConn
	addr string
	// validUntil is when the first penalty among the READY nodes ends, zero if none is penalized.
	validUntil time.Time
}

// rank returns the READY nodes in the order to pick them at now:
//
//	unpenalized nodes  first, by configured order
//	penalized nodes    last, oldest failure first, then by configured order
//
// A penalty ends at penalizedUntil. Penalized nodes are not sorted by the end of their penalty because unary calls and
// streams penalize for different durations: sorting by failure sends every failing node to the back.
func rank(nodes []*node, now time.Time) []*node {
	ready := slices.DeleteFunc(slices.Clone(nodes), func(n *node) bool { return n.sc == nil })
	slices.SortFunc(ready, func(a, b *node) int {
		aPenalized, bPenalized := now.Before(a.penalizedUntil), now.Before(b.penalizedUntil)
		switch {
		case aPenalized && !bPenalized:
			return 1
		case !aPenalized && bPenalized:
			return -1
		case aPenalized:
			return cmp.Or(a.failedAt.Compare(b.failedAt), cmp.Compare(a.order, b.order))
		}
		return cmp.Compare(a.order, b.order)
	})
	return ready
}

// refresh stores the choice ranked at now. It is the only writer of current, and fb.mu must be held.
func (fb *fallbackBalancer) refresh(now time.Time) {
	ranked := rank(slices.Collect(maps.Values(fb.nodes)), now)
	if len(ranked) == 0 {
		fb.current.Store(nil)
		return
	}
	c := &choice{sc: ranked[0].sc, addr: ranked[0].addr}
	for _, n := range ranked {
		if now.Before(n.penalizedUntil) && (c.validUntil.IsZero() || n.penalizedUntil.Before(c.validUntil)) {
			c.validUntil = n.penalizedUntil
		}
	}
	fb.current.Store(c)
}

// choose returns the current choice, refreshed first if a penalty ended since it was made.
func (fb *fallbackBalancer) choose() *choice {
	c := fb.current.Load()
	if c == nil || c.validUntil.IsZero() || fb.now().Before(c.validUntil) {
		return c
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.refresh(fb.now())
	return fb.current.Load()
}

// penalize puts the node at addr behind the others for at least d, whether or not it is READY.
func (fb *fallbackBalancer) penalize(addr string, d time.Duration) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	n, ok := fb.nodes[addr]
	if !ok {
		// the resolver removed it
		return
	}
	now := fb.now()
	fbLog.Warning("penalizing node", "addr", addr, "for", d)
	n.failedAt = now
	if until := now.Add(d); until.After(n.penalizedUntil) {
		n.penalizedUntil = until
	}
	fb.refresh(now)
}

// UpdateClientConnState keeps one node per address before the base balancer creates their SubConns.
func (fb *fallbackBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	fbLog.Info("Called UpdateClientConnState with addresses", len(s.ResolverState.Addresses))
	// Without addresses, the base balancer shuts every SubConn down, reports a resolver error and returns
	// balancer.ErrBadResolverState.
	s.ResolverState.Addresses = fb.setAddresses(s.ResolverState.Addresses)
	return fb.Balancer.UpdateClientConnState(s)
}

// setAddresses reconciles the nodes with addrs, and returns addrs without the ones that lack an order or repeat an
// address: a node only keeps its lowest order, and one SubConn.
func (fb *fallbackBalancer) setAddresses(addrs []resolver.Address) []resolver.Address {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	kept := make([]resolver.Address, 0, len(addrs))
	orders := make(map[string]int, len(addrs))
	for _, a := range addrs {
		order, ok := a.Attributes.Value("order").(int)
		if !ok {
			fbLog.Error("invalid order attribute on Address, make sure to use a compatible resolver", "addr", a)
			continue
		}
		if prev, dup := orders[a.Addr]; dup {
			if order < prev {
				orders[a.Addr] = order
				kept[slices.IndexFunc(kept, func(k resolver.Address) bool { return k.Addr == a.Addr })] = a
			}
			continue
		}
		orders[a.Addr] = order
		kept = append(kept, a)
	}
	for addr, n := range fb.nodes {
		order, ok := orders[addr]
		if !ok {
			delete(fb.nodes, addr)
			continue
		}
		n.order = order
	}
	for addr, order := range orders {
		if _, ok := fb.nodes[addr]; !ok {
			fb.nodes[addr] = &node{addr: addr, order: order}
		}
	}
	fb.refresh(fb.now())
	return kept
}

// Build implements base.PickerBuilder. The base balancer calls it whenever a SubConn enters or leaves READY, except
// when no SubConn is left READY, CONNECTING or IDLE: failureWatcher covers that case.
func (fb *fallbackBalancer) Build(info base.PickerBuildInfo) balancer.Picker {
	ready := make(map[string]balancer.SubConn, len(info.ReadySCs))
	for sc, sci := range info.ReadySCs {
		ready[sci.Address.Addr] = sc
	}
	fb.setReady(ready)
	return &picker{fb: fb}
}

// setReady records which SubConn, if any, is READY for each node.
func (fb *fallbackBalancer) setReady(ready map[string]balancer.SubConn) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for addr, n := range fb.nodes {
		if (n.sc == nil) != (ready[addr] == nil) {
			fbLog.Info("node readiness changed", "addr", addr, "ready", ready[addr] != nil)
		}
		n.sc = ready[addr]
	}
	fb.refresh(fb.now())
}

// failureWatcher clears the READY nodes when the base balancer reports TRANSIENT_FAILURE, because it then installs
// its own error picker without calling Build.
type failureWatcher struct {
	balancer.ClientConn
	fb *fallbackBalancer
}

func (w failureWatcher) UpdateState(s balancer.State) {
	if s.ConnectivityState == connectivity.TransientFailure {
		w.fb.setReady(nil)
	}
	w.ClientConn.UpdateState(s)
}

type picker struct {
	fb *fallbackBalancer
}

func (p *picker) String() string {
	if c := p.fb.current.Load(); c != nil {
		return c.addr
	}
	return "no READY node"
}

// SkipCtxKey, set to true in a call's context, penalizes the node the call would have used and picks the next one.
type SkipCtxKey struct{}

// streamPenaltyKey marks a call whose every failure is the node's fault. Its time.Duration value is how long the
// node stays behind the others, which must outlast the time the caller takes to retry.
type streamPenaltyKey struct{}

func (p *picker) Pick(info balancer.PickInfo) (balancer.PickResult, error) {
	fb := p.fb
	if skip, _ := info.Ctx.Value(SkipCtxKey{}).(bool); skip {
		if c := fb.choose(); c != nil {
			fb.penalize(c.addr, fb.timeout)
		}
	}
	c := fb.choose()
	if c == nil {
		fbLog.Error("Pick had no available SubConn")
		return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
	}
	penalty, stream := info.Ctx.Value(streamPenaltyKey{}).(time.Duration)
	if !stream {
		penalty = fb.timeout
	}

	// The metric for a subchannel should be atomically incremented by one
	// after it has been successfully picked by the picker
	RequestsCounter.With(prometheus.Labels{"node": c.addr}).Inc()
	fbLog.Info("Picked SubConn", "addr", c.addr)
	return balancer.PickResult{
		SubConn: c.sc,
		Done: func(di balancer.DoneInfo) {
			if nodeFault(info.Ctx, di.Err, stream) {
				fb.penalize(c.addr, penalty)
			}
		},
		Metadata: metadata.MD{"target": []string{c.addr}},
	}, nil
}

// nodeFault reports whether err means the node failed the call, rather than the caller giving up or asking for
// something no node can serve. A stream we cancelled because the node stopped emitting beacons is the node's fault.
// drand nodes report unknown chains and rounds as codes.Unknown, so outside of a stream only transport-level codes
// count.
func nodeFault(ctx context.Context, err error, stream bool) bool {
	if err == nil {
		return false
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return errors.Is(context.Cause(ctx), errStreamStalled)
	}
	if stream {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.ResourceExhausted, codes.Unimplemented:
		return true
	}
	return false
}

// UsedEndpointInterceptor is a gRPC client-side interceptor that provides reporting for which endpoint is being used by each RPC.
func UsedEndpointInterceptor(l logger) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		usedEndpoint := grpc.PeerCallOption{PeerAddr: &peer.Peer{}}
		opts = append(opts, usedEndpoint)
		err := invoker(ctx, method, req, reply, cc, opts...)
		l.Debug("Fallback UsedEndpointInterceptor", "method", method, "remote", usedEndpoint.PeerAddr.String())
		return err
	}
}
