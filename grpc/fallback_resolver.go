package grpc

import (
	"strings"

	"google.golang.org/grpc/resolver"
)

type FallbackResolver struct {
	target resolver.Target
	cc     resolver.ClientConn
}

func (*FallbackResolver) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	r := &FallbackResolver{
		target: target,
		cc:     cc,
	}
	r.start()
	return r, nil
}

func (*FallbackResolver) Scheme() string {
	return "fallback"
}

func (r *FallbackResolver) start() {
	addrStrs := strings.Split(r.target.Endpoint(), ",")
	addrs := make([]resolver.Address, len(addrStrs))
	for i, a := range addrStrs {
		addrs[i] = resolver.Address{Addr: a}
	}
	r.cc.UpdateState(resolver.State{Addresses: addrs})
}

func (*FallbackResolver) ResolveNow(o resolver.ResolveNowOptions) {}

func (*FallbackResolver) Close() {}
