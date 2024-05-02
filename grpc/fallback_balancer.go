package grpc

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
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

func init() {
	balancer.Register(fallbackBB{})
}

// LBConfig is the balancer config for pick_first_with_fallback balancer.
type LBConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`

	// FallbackSeconds is the number of seconds the fallbackBalancer should wait
	// before attempting to fallback to its first endpoints. If set to 0, it behaves like the pick_first balancer.
	FallbackSeconds uint32 `json:"fallbackSeconds,omitempty"`
}

type fallbackBB struct{}

func (fallbackBB) ParseConfig(s json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	lbConfig := &LBConfig{
		FallbackSeconds: 3,
	}
	if err := json.Unmarshal(s, lbConfig); err != nil {
		return nil, fmt.Errorf("least-request: unable to unmarshal LBConfig: %v", err)
	}

	return lbConfig, nil
}

func (fallbackBB) Name() string {
	return fallbackName
}

func (fallbackBB) Build(cc balancer.ClientConn, bOpts balancer.BuildOptions) balancer.Balancer {
	b := &fallbackBalancer{scAddrs: make(map[balancer.SubConn]*scWithAddr)}
	baseBuilder := base.NewBalancerBuilder(fallbackName, b,
		base.Config{
			//TODO: understand better whether or not we want to rely on the built-in grpc HealthCheck
			HealthCheck: false,
		})
	b.Balancer = baseBuilder.Build(cc, bOpts)
	return b
}

type fallbackBalancer struct {
	// Embeds balancer.Balancer because needs to intercept UpdateClientConnState to learn state.
	balancer.Balancer

	scAddrs map[balancer.SubConn]*scWithAddr // Hold onto SubConn address to keep track for subsequent picker updates.
}

func (fb *fallbackBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	addrs := s.ResolverState.Addresses
	fbLog.Info("Called UpdateClientConnState with addresses", len(addrs))
	if len(addrs) == 0 {
		// The resolver reported an empty address list.
		// Treat it like an error by calling b.ResolverError.
		// In theory this should never happen with our own custom resolver
		for sc, addr := range fb.scAddrs {
			// Shut down the old subConn. All addresses were removed, so it is no longer valid.
			sc.Shutdown()
			fbLog.Info("shutting down SubConn", "addr", addr)
			delete(fb.scAddrs, sc)
		}
		fbLog.Warning("resolver provided zero addresses")
		// it is important to shutdown all SubConn above since ResolverError relies on len(ReadySC) to re-resolve
		fb.Balancer.ResolverError(fmt.Errorf("resolver provided zero addresses"))
		return balancer.ErrBadResolverState
	}

	return fb.Balancer.UpdateClientConnState(s)
}

type scWithAddr struct {
	sc    balancer.SubConn
	addr  string
	ready bool
	order int
}

func (s *scWithAddr) String() string {
	return fmt.Sprintf("%d-%s-%v", s.order, s.addr, s.ready)
}

// scCmp should return 0 if the slice element s matches
// the target t, a negative number if the slice element s precedes the target t,
// or a positive number if the slice element s follows the target t.
// cmp must implement the same ordering as the slice, such that if
// cmp(a, t) < 0 and cmp(b, t) >= 0, then a must precede b in the slice.
func scCmp(s, t *scWithAddr) int {
	return s.order - t.order
}

// insert will insert s in scs in a sorted way, relying on the above comparison function. It will be in ascending order.
func insert(scs []*scWithAddr, s *scWithAddr) []*scWithAddr {
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

	for sc, sca := range fb.scAddrs {
		if _, ok := info.ReadySCs[sc]; !ok {
			// If no longer ready, we are in a degraded state!
			// This might mean an endpoint changed their resolved IP, which we do not handle gracefully yet
			// but most likely means a connection is failing temporarily and should be checked again soon.
			// We rely on the grpc built-in reconnect backoff process to re-trigger
			fbLog.Warning("SubConn not ready anymore", "addr", sca.addr)
			sca.ready = false
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
			sc:    sc,
			addr:  addr.Address.Addr,
			ready: true,
			order: order,
		}

		fbLog.Info("Processing Ready SubConn", "addr", addr.Address, "order", order)
		if oldAddr, ok := fb.scAddrs[sc]; !ok {
			fb.scAddrs[sc] = sca
		} else if oldAddr != fb.scAddrs[sc] {
			fbLog.Error("Conflicting SubConn", "oldAddr", oldAddr, "newAddr", fb.scAddrs[sc])
		}

		// we insert in correct order, by priority
		scs = insert(scs, sca)
	}

	fbLog.Error("Prepared readySCs", "scs", scs)

	return &picker{
		subConns: scs,
	}
}

type picker struct {
	// Built out when receives list of ready RPCs.
	subConns []*scWithAddr
}

type SkipCtxKey struct{}

func (p *picker) Pick(b balancer.PickInfo) (balancer.PickResult, error) {
	var pickedSC *scWithAddr

	// we rely on the 0 value of int being 0 when the key isn't set
	skip, _ := b.Ctx.Value(SkipCtxKey{}).(int)
	var skipped int

	// subConns are in the priority order set by the resolver
	for _, sc := range p.subConns {
		fbLog.Info("considering to pick", "addr", sc.addr, "order", sc.order, "ready", sc.ready)
		if sc.ready {
			if skip > 0 {
				fbLog.Error("skipping SubConn", "addr", sc.addr, "order", sc.order)
				skip--
				skipped++
				continue
			}
			pickedSC = sc
			break
		}
	}

	if pickedSC == nil {
		fbLog.Error("Pick had no available SubConn", "subConnes", p.subConns, "skipped", skipped)
		return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
	}

	// The metric for a subchannel should be atomically incremented by one
	// after it has been successfully picked by the picker
	RequestsCounter.With(prometheus.Labels{"node": pickedSC.addr}).Inc()
	fbLog.Info("Picked SubConn", "addr", pickedSC.addr, "skipped", skipped)
	return balancer.PickResult{
		SubConn: pickedSC.sc,
		//// We could define a Done function on the PickResults too
		//Done: func(info balancer.DoneInfo) {},
		Metadata: metadata.MD{"target": []string{pickedSC.addr}},
	}, nil
}
