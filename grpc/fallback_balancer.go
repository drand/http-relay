package grpc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/grpclog"
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

var grpclogger = grpclog.Component("fallback_picker")

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
	b := &fallbackBalancer{scAddrs: make(map[balancer.SubConn]string)}
	baseBuilder := base.NewBalancerBuilder(fallbackName, b, base.Config{HealthCheck: true})
	b.Balancer = baseBuilder.Build(cc, bOpts)
	return b
}

type fallbackBalancer struct {
	// Embeds balancer.Balancer because needs to intercept UpdateClientConnState to learn state.
	balancer.Balancer

	fallbackSeconds uint32
	scAddrs         map[balancer.SubConn]string // Hold onto SubConn address to keep track for subsequent picker updates.
}

func (fb *fallbackBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	lrCfg, ok := s.BalancerConfig.(*LBConfig)
	if !ok {
		slog.Error("pick_first_with_fallback: received config with unexpected type %T: %v", s.BalancerConfig, s.BalancerConfig)
		lrCfg = &LBConfig{
			FallbackSeconds: 0,
		}
	}

	addrs := s.ResolverState.Addresses
	if len(addrs) == 0 {
		// The resolver reported an empty address list. Treat it like an error by
		// calling b.ResolverError.
		for sc, addr := range fb.scAddrs {
			// Shut down the old subConn. All addresses were removed, so it is
			// no longer valid.
			sc.Shutdown()
			slog.Warn("shutting down SubConn", "addr", addr)
			delete(fb.scAddrs, sc)
		}
		slog.Warn("resolver provided zero addresses")
		fb.Balancer.ResolverError(fmt.Errorf("resolver provided zero addresses"))
		return balancer.ErrBadResolverState
	}

	fb.fallbackSeconds = lrCfg.FallbackSeconds

	return fb.Balancer.UpdateClientConnState(s)
}

type scWithAddr struct {
	sc    balancer.SubConn
	addr  string
	ready bool
	order int
}

func scCmp(a, b scWithAddr) int {
	return b.order - a.order // we inverted the order to have a list in descending order
}

func insert(scs []scWithAddr, s scWithAddr) []scWithAddr {
	i, _ := slices.BinarySearchFunc(scs, s, scCmp) // find slot
	return slices.Insert(scs, i, s)
}

// Build is implementing the base.PickerBuilder interface, expecting a ReadySCs list of SubConn that are ready to
// be used, and we build our list of addresses using it.
func (fb *fallbackBalancer) Build(info base.PickerBuildInfo) balancer.Picker {
	slog.Debug("fallbackPicker built called", "#readySC", len(info.ReadySCs))
	if len(info.ReadySCs) == 0 {
		return base.NewErrPicker(balancer.ErrNoSubConnAvailable)
	}

	for sc := range fb.scAddrs {
		if _, ok := info.ReadySCs[sc]; !ok { // If no longer ready, no more need for it.
			sc.Shutdown()
			delete(fb.scAddrs, sc)
		}
	}

	scs := make([]scWithAddr, 0, len(info.ReadySCs))
	// Create new refs if needed and add them to picker list in correct order based on their attribute
	for sc, addr := range info.ReadySCs {
		order, ok := addr.Address.Attributes.Value("order").(int)
		if !ok {
			slog.Error("invalid order attribute on Address, make sure to use a compatible resolver", "addr", addr)
			continue
		}

		slog.Warn("Got SubConn with order", "order", order)
		if _, ok := fb.scAddrs[sc]; !ok {
			fb.scAddrs[sc] = addr.Address.String()
		}

		// we insert in correct order
		scs = insert(scs, scWithAddr{
			sc:    sc,
			addr:  addr.Address.String(),
			ready: true,
			order: order,
		})
	}

	slog.Warn("Prepared readySCs", "scs", scs)

	return &picker{
		subConns: scs,
	}
}

type picker struct {
	// Built out when receives list of ready RPCs.
	subConns []scWithAddr
}

func (p *picker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	var pickedSC *scWithAddr

	for _, sc := range p.subConns {
		if sc.ready {
			pickedSC = &sc
		}
	}

	if pickedSC == nil {
		return balancer.PickResult{}, balancer.ErrNoSubConnAvailable
	}

	// The metric for a subchannel should be atomically incremented by one
	// after it has been successfully picked by the picker
	RequestsCounter.With(prometheus.Labels{"node": pickedSC.addr}).Inc()
	slog.Warn("chose sc", "addr", pickedSC.addr)
	return balancer.PickResult{
		SubConn: pickedSC.sc,
		Done: func(info balancer.DoneInfo) {
			// when a SubConn is not ready, it will call Done with a nil DoneInfo
			if info.BytesReceived == false && info.BytesSent == false && info.Err == nil {
				slog.Warn("SubConn not ready anymore", "addr", pickedSC.addr)
				pickedSC.ready = false
			}
		},
	}, nil
}
