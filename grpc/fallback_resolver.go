package grpc

import (
	"log/slog"
	"strings"

	"google.golang.org/grpc/resolver"
)

func init() {
	resolver.Register(&FallbackResolver{})
}

// FallbackResolver implements both resolver.Resolver and resolver.Builder since there is no special handling required
// when building one. Most notably, it currently doesn't support any resolver.BuildOptions.
type FallbackResolver struct {
	target resolver.Target
	cc     resolver.ClientConn
}

func (*FallbackResolver) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	r := &FallbackResolver{
		target: target,
		cc:     cc,
	}

	return r, r.start()
}

const FallbackResolverName = "fallback"

func (*FallbackResolver) Scheme() string {
	return FallbackResolverName
}

func (r *FallbackResolver) start() error {
	addrStrs := strings.Split(r.target.Endpoint(), ",")
	addrs := make([]resolver.Address, len(addrStrs))
	for i, a := range addrStrs {
		slog.Info("Adding backend address to pool", "host", a)
		addrs[i] = resolver.Address{Addr: a, ServerName: a}
	}
	// If a resolver sets Addresses but does not set Endpoints, one Endpoint
	// will be created for each Address before the State is passed to the LB
	// policy.
	return r.cc.UpdateState(resolver.State{Addresses: addrs})
}

func (*FallbackResolver) ResolveNow(o resolver.ResolveNowOptions) {}

func (*FallbackResolver) Close() {}
