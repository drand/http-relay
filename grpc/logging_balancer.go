package grpc

import (
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/resolver"
)

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
	b.log.Info("building logging balancer", "sub-balancer", b.sub, "connTarget", cc.Target(), "BuildOptions Target", opt.Target.String())
	return &loggingBalancer{sub: balancer.Get(b.sub).Build(&wrappedClientConn{cc, cc.Target(), b.log}, opt), log: b.log}
}

// Name will return the name of the underlying balancer used during registration with NewLoggingBalancerBuilder,
// prepended with "logging_".
func (b *loggingBalancerBuilder) Name() string {
	return "logging_" + b.sub
}

type loggingBalancer struct {
	sub balancer.Balancer
	log logger
}

func (b *loggingBalancer) UpdateClientConnState(in balancer.ClientConnState) error {
	b.log.Debug("UpdateClientConnState started", "address_len", len(in.ResolverState.Addresses))
	err := b.sub.UpdateClientConnState(in)
	if err != nil {
		b.log.Error("UpdateClientConnState errored", "err", err)
		return err
	}

	b.log.Debug("UpdateClientConnState done")
	return nil
}

func (b *loggingBalancer) ResolverError(err error) {
	b.log.Error("ResolverError", "err", err)
	b.sub.ResolverError(err)
}

func (b *loggingBalancer) UpdateSubConnState(cc balancer.SubConn, state balancer.SubConnState) {
	b.log.Debug("UpdateSubConnState", "state", state.ConnectivityState.String())
	b.sub.UpdateSubConnState(cc, state)
}

func (b *loggingBalancer) Close() {
	b.log.Info("Closing logging balancer", "sub-balancer", b.sub)
	b.sub.Close()
}

type logPicker struct {
	sub balancer.Picker
	log logger
}

func (p *logPicker) Pick(i balancer.PickInfo) (balancer.PickResult, error) {
	result, err := p.sub.Pick(i)

	p.log.Debug("Pick", "endpoint", result.SubConn, "rpc", i.FullMethodName, "md", result.Metadata, "err", err)
	return balancer.PickResult{
		SubConn: result.SubConn,
		Done: func(info balancer.DoneInfo) {
			if info.Err == nil && info.BytesSent == info.BytesReceived && info.BytesSent == false {
				p.log.Error("Picker subconn not ready")
				return
			}
			if info.Err != nil {
				p.log.Error("Picked SubConn errored", "err", info.Err)
			}
			result.Done(info)
		},
		Metadata: result.Metadata,
	}, err
}

type wrappedClientConn struct {
	balancer.ClientConn
	name string
	log  logger
}

func (w *wrappedClientConn) UpdateState(s balancer.State) {
	w.log.Info("Balancer ClientConn state update", "state", s, "name", w.name)
	w.ClientConn.UpdateState(balancer.State{
		ConnectivityState: s.ConnectivityState,
		Picker: &logPicker{
			sub: s.Picker,
			log: w.log,
		},
	})
}

func (w *wrappedClientConn) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	// in a future release, NewSubConn will only support a single address, so let's make sure we do that already.
	addr := addrs[0]
	w.log.Debug("NewSubConn called", "addrs", len(addrs), "first", addr.Addr)
	nOpts := balancer.NewSubConnOptions{
		CredsBundle:        opts.CredsBundle,
		HealthCheckEnabled: opts.HealthCheckEnabled,
		StateListener: func(state balancer.SubConnState) {
			w.log.Debug("StateListener", "addr", addr.Addr, "state", state.ConnectivityState.String())
			opts.StateListener(state)
		},
	}
	sb, err := w.ClientConn.NewSubConn([]resolver.Address{addr}, nOpts)
	if err != nil {
		w.log.Error("NewSubConn errored", "err", err)
	}
	return sb, err
}
