package grpc

import (
	"encoding/json"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

//func init() {
//	balancer.Register(fallbackBuilder{})
//}

const fallbackName = "pick_first_with_fallback"

type fallbackConfig struct {
	serviceconfig.LoadBalancingConfig `json:"-"`

	// FallbackSecond represents how often pick should try again to choose the first
	// SubConn in the list. Defaults to 3. If 0, it never fallbacks to failing SubConn
	// effectively behaving like pick_first.
	FallbackSecond uint32 `json:"fallbackSecond,omitempty"`
}

type fallbackBuilder struct{}

func (fallbackBuilder) ParseConfig(s json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
	lbConfig := &fallbackConfig{
		FallbackSecond: 3,
	}

	if err := json.Unmarshal(s, lbConfig); err != nil {
		return nil, fmt.Errorf("fallback-balancer: unable to unmarshal fallbackConfig: %v", err)
	}
	return lbConfig, nil
}

func (fallbackBuilder) Name() string {
	return fallbackName
}

//func (fallbackBuilder) Build(cc balancer.ClientConn, bOpts balancer.BuildOptions) balancer.Balancer {
//	f := &fallback{
//		ClientConn: cc,
//		bOpts:      bOpts,
//	}
//	// we still use the pick_first balancer as the underlying balancer
//	f.Balancer = base.NewBalancer(f, bOpts)
//	return f
//}
//
//type fallback struct {
//}
