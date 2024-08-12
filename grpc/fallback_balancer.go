package grpc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/serviceconfig"
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

// LBConfig is the balancer config for pick_first_with_fallback balancer.
type LBConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`

	// FallbackSeconds is the number of seconds the fallbackBalancer should wait
	// before attempting to fallback to its first endpoints. If set to 0, it behaves like the pick_first balancer.
	FallbackSeconds uint32 `json:"fallbackSeconds,omitempty"`
}

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
	b := &fallbackBalancer{scAddrs: make(map[balancer.SubConn]*scWithAddr), closing: make(chan struct{})}
	// we delegate the actual SubConn management to the base balancer
	baseBuilder := base.NewBalancerBuilder(fallbackName, b,
		base.Config{
			//TODO: understand better whether or not we want to rely on the built-in grpc HealthCheck
			HealthCheck: false,
		})
	b.Balancer = baseBuilder.Build(cc, bOpts)
	go b.runBackgroundTimer(f.timeout)
	return b
}

type fallbackBalancer struct {
	// Embeds balancer.Balancer because needs to intercept UpdateClientConnState to learn state.
	balancer.Balancer

	mu sync.RWMutex

	scAddrs map[balancer.SubConn]*scWithAddr // Hold onto SubConn address to keep track for subsequent picker updates.
	closing chan struct{}
}

// Function to continuously process updates
func (fb *fallbackBalancer) runBackgroundTimer(timeout time.Duration) {
	ticker := time.NewTimer(timeout)
	for {
		select {
		case <-ticker.C:
			fb.mu.Lock()
			// every tick, we reset the priority of all subconn, the ones whose underlying SubConn is actually not READY
			// are deleted from that list anyway in UpdateClientConnState.
			for _, sc := range fb.scAddrs {
				sc.ResetPriority()
			}
			fb.mu.Unlock()
			ticker.Reset(timeout)
		case <-fb.closing:
			fbLog.Info("received a Close, shutting down fallback balancer")
			fb.mu.Lock()
			ticker.Stop()
			// we empty the balancer
			for sc := range fb.scAddrs {
				delete(fb.scAddrs, sc)
			}
			fb.mu.Unlock()
			// now we call the underlying balancer Close() managing the actual SubConn
			// this is the one that will be calling Shutdown on each SubConn
			// as will be mandated in a future go-grpc release.
			fb.Balancer.Close()
			return
		}
	}
}

func (fb *fallbackBalancer) Close() {
	close(fb.closing)
}

func (fb *fallbackBalancer) dec(sc balancer.SubConn) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if sca, ok := fb.scAddrs[sc]; !ok {
		fbLog.Error("trying to update state on a non-existent subconn", "subconn", sc)
	} else {
		// -2 to increase the chance of having another higher priority conn available
		sca.UpdatePriority(-2)
	}
}

// first returns the subconn with the highest priority among the ones available.
// It can return a nil subconn if none are ready, this must be handled by the caller.
func (fb *fallbackBalancer) first() *scWithAddr {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	// quick path for the case with a single subconn
	if len(fb.scAddrs) == 1 {
		for _, sca := range fb.scAddrs {
			return sca
		}
	}
	// in case scAddrs is empty, we need at least 1 nil and 1 cap
	ret := make([]*scWithAddr, 1, len(fb.scAddrs)+1)
	for _, sca := range fb.scAddrs {
		// we insert in correct order, by priority
		ret = insert(ret, sca)
	}

	return ret[0]
}

func (fb *fallbackBalancer) second() *scWithAddr {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	// quick path for the case with a single subconn or none
	if len(fb.scAddrs) < 2 {
		return nil
	}
	ret := make([]*scWithAddr, 0, len(fb.scAddrs))
	for _, sca := range fb.scAddrs {
		// we insert in correct order, by priority
		ret = insert(ret, sca)
	}

	return ret[1]
}

func (fb *fallbackBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	addrs := s.ResolverState.Addresses
	fbLog.Info("Called UpdateClientConnState with addresses", len(addrs))
	if len(addrs) == 0 {
		fb.mu.Lock()
		// The resolver reported an empty address list.
		// Treat it like an error by calling b.ResolverError.
		// In theory this should never happen with our own custom resolver
		for sc, addr := range fb.scAddrs {
			// Shut down the old subConn. All addresses were removed, so it is no longer valid.
			sc.Shutdown()
			fbLog.Info("shutting down SubConn", "addr", addr)
			delete(fb.scAddrs, sc)
		}
		fb.mu.Unlock()
		fbLog.Warning("resolver provided zero addresses")
		// TODO: is this true?
		// it's important to shutdown all SubConn above since ResolverError relies on len(ReadySC) to re-resolve
		fb.Balancer.ResolverError(fmt.Errorf("resolver provided zero addresses"))
		return balancer.ErrBadResolverState
	}

	return fb.Balancer.UpdateClientConnState(s)
}

type scWithAddr struct {
	sc balancer.SubConn
	// the underlying target's address
	addr string
	// whether this SubConn is currently considered ready or not. Negative priority disables it.
	priority int
	// order is used to prioritize the SubConn to use, a negative one leads to it not being used at all
	order int

	// we can have concurrent updates of the priority, so we need to guard our scWithAddr with a mutex
	mu sync.RWMutex
}

func (s *scWithAddr) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("%d/%d-%s", s.priority, s.order, s.addr)
}

func (s *scWithAddr) ResetPriority() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priority = s.order
}

func (s *scWithAddr) UpdatePriority(update int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priority += update
}

// scCmp should return 0 if the slice element s matches
// the target t, a negative number if the slice element s precedes the target t,
// or a positive number if the slice element s follows the target t.
// cmp must implement the same ordering as the slice, such that if
// cmp(a, t) < 0 and cmp(b, t) >= 0, then a must precede b in the slice.
// For nil values, we consider them "larger" than anything
func scCmp(s, t *scWithAddr) int {
	if s == nil {
		return 1
	} else if t == nil {
		return -1
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.priority - t.priority
}

// insert will insert s in scs in a sorted way, relying on the above comparison function. It will be in ascending order.
func insert(scs []*scWithAddr, s *scWithAddr) []*scWithAddr {
	if s == nil {
		return scs
	}
	i, _ := slices.BinarySearchFunc(scs, s, scCmp) // find slot
	return slices.Insert(scs, i, s)
}

// Build is implementing the base.PickerBuilder interface, expecting a ReadySCs list of SubConn that are ready to
// be used, and we build our list of addresses using it.
func (fb *fallbackBalancer) Build(info base.PickerBuildInfo) balancer.Picker {
	fbLog.Info("fallbackPicker built called", "#readySC", len(info.ReadySCs))
	if len(info.ReadySCs) == 0 {
		return base.NewErrPicker(balancer.ErrNoSubConnAvailable)
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	for sc, sca := range fb.scAddrs {
		if _, ok := info.ReadySCs[sc]; !ok {
			// This might mean an endpoint changed their resolved IP, which we do not handle gracefully yet
			// but most likely means a connection is failing temporarily.
			// We rely on the grpc built-in reconnect backoff process to re-trigger this through the baseBalancer.
			fbLog.Warning("SubConn not ready anymore", "addr", sca.addr)
			delete(fb.scAddrs, sc)
		}
	}

	scs := make([]*scWithAddr, 0, len(info.ReadySCs))
	// Create new refs if needed and add them to picker list in correct order based on their attribute
	for sc, addr := range info.ReadySCs {
		order, ok := addr.Address.Attributes.Value("order").(int)
		if !ok {
			fbLog.Error("invalid order attribute on Address, make sure to use a compatible resolver", "addr", addr)
			continue
		}

		sca := &scWithAddr{
			sc:       sc,
			addr:     addr.Address.Addr,
			priority: order,
			order:    order,
		}

		fbLog.Info("Processing Ready SubConn", "addr", addr.Address, "order", order)
		// we replace the sca in our LB in case its addr or order was changed
		fb.scAddrs[sc] = sca
	}

	fbLog.Info("Prepared fallback LB picker with ready SubConns", "scs", scs)

	return &picker{
		fb: fb,
	}
}

type picker struct {
	fb *fallbackBalancer
}

func (p *picker) String() string {
	if p == nil || p.fb == nil {
		return "nil"
	}
	var sb strings.Builder
	sb.WriteString("[ ")
	first := p.fb.first()
	if first != nil {
		sb.WriteString(first.String() + "; ")
	}
	sb.WriteString("]")
	return sb.String()
}

type SkipCtxKey struct{}

func (p *picker) Pick(b balancer.PickInfo) (balancer.PickResult, error) {
	// we rely on the 0 value of int being 0 when the key isn't set
	skip, _ := b.Ctx.Value(SkipCtxKey{}).(bool)

	picked := p.fb.first()
	fbLog.Info("considering to pick", "first", picked, "skip", skip)
	// we got a skip context, so we'll try to see if there is a next subconn
	if skip {
		second := p.fb.second()
		if second != nil {
			fbLog.Error("skipping & deprioritizing first SubConn for now", "addr", picked.addr)
			p.fb.dec(picked.sc)
			picked = second
		}
	}

	if picked == nil {
		fbLog.Error("Pick had no available SubConn", "skipped", skip)
		return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
	}

	// The metric for a subchannel should be atomically incremented by one
	// after it has been successfully picked by the picker
	RequestsCounter.With(prometheus.Labels{"node": picked.addr}).Inc()
	fbLog.Info("Picked SubConn", "addr", picked.addr, "skipped", skip)
	return balancer.PickResult{
		SubConn: picked.sc,
		Done: func(info balancer.DoneInfo) {
			if info.Err != nil {
				p.fb.dec(picked.sc)
			}
		},
		Metadata: metadata.MD{"target": []string{picked.addr}},
	}, nil
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
