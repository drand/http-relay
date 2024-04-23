package grpc

import (
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/resolver"
)

const loggingBalancerName = "logging_balancer"

type loggingBalancerBuilder struct {
	sub string
	log logger
}

// NewLoggingBalancerBuilder is meant to be used along with balancer.Register in order to create a custom logging
// balancer that will log all functionalities used with the provided logger, using the underlying balancer. This
// should be called in your init function and is not thread safe.
func NewLoggingBalancerBuilder(balancerName string, l logger) balancer.Builder {
	return &loggingBalancerBuilder{sub: balancerName, log: l}
}

func (b *loggingBalancerBuilder) Build(cc balancer.ClientConn, opt balancer.BuildOptions) balancer.Balancer {
	//b.log.Info("building logging balancer", "sub-balancer", b.sub, "connTarget", cc.Target(), "BuildOptions Target", opt.Target.String())
	return &loggingBalancer{sub: balancer.Get(b.sub).Build(&wrappedClientConn{cc, cc.Target(), b.log}, opt), log: b.log}
}

func (b *loggingBalancerBuilder) Name() string {
	return "logging_" + b.sub
}

type loggingBalancer struct {
	sub balancer.Balancer
	log logger
}

func (b *loggingBalancer) UpdateClientConnState(in balancer.ClientConnState) error {
	b.log.Info("updating logging balancer", "state", in.ResolverState)
	return b.sub.UpdateClientConnState(in)
}
func (b *loggingBalancer) ResolverError(err error) {
	b.log.Error("balancer resolver error", err)
	b.sub.ResolverError(err)
}

func (b *loggingBalancer) UpdateSubConnState(cc balancer.SubConn, state balancer.SubConnState) {
	b.log.Info("balancer update sub conn state", "state", state.ConnectivityState.String())
	b.sub.UpdateSubConnState(cc, state)
}

func (b *loggingBalancer) Close() {
	b.log.Info("balancer Close called")
	b.sub.Close()
}

type logPicker struct {
	sub balancer.Picker
	log logger
}

func (p *logPicker) Pick(i balancer.PickInfo) (balancer.PickResult, error) {
	result, err := p.sub.Pick(i)
	p.log.Info("pick chose", "endpoint", result.SubConn, "metadata", result.Metadata, "rpc", i.FullMethodName)
	return result, err
}

//func (p *picker) String() string {
//	return p.
//}

type wrappedClientConn struct {
	balancer.ClientConn
	name string
	log  logger
}

func (w *wrappedClientConn) UpdateState(s balancer.State) {
	w.log.Info("updating state", "state", s, "name", w.name)
	w.ClientConn.UpdateState(balancer.State{
		ConnectivityState: s.ConnectivityState,
		Picker: &logPicker{
			sub: s.Picker,
			log: w.log,
		},
	})
}

func (w *wrappedClientConn) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	addr := addrs[0]
	w.log.Warn("NewSubConn called", "addrs", len(addrs), "first", addr)
	nOpts := balancer.NewSubConnOptions{
		CredsBundle:        opts.CredsBundle,
		HealthCheckEnabled: opts.HealthCheckEnabled,
		StateListener: func(state balancer.SubConnState) {
			w.log.Debug("StateListener", "addr", addr, "state", state)
			opts.StateListener(state)
		},
	}
	sb, err := w.ClientConn.NewSubConn([]resolver.Address{addr}, nOpts)
	if err != nil {
		return sb, err
	}
	return sb, err
}
