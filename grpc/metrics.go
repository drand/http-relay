package grpc

import (
	"context"
	"log/slog"
	"net"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/channelz/grpc_channelz_v1"
	"google.golang.org/grpc/channelz/service"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// ClientMetrics about the drand client requests to servers
	ClientMetrics = prometheus.NewRegistry()

	grpcServerCallsFailedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "grpc_server_calls_failed_total",
		Help: "The total number of gRPC server failed calls.",
	}, []string{"target"})

	grpcServerCallsStartedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "grpc_server_calls_started_total",
		Help: "The total number of gRPC server calls started.",
	}, []string{"target"})

	grpcServerLastCallStartedSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "grpc_server_last_call_started_seconds",
		Help: "The timestamp of the last gRPC call started.",
	}, []string{"target"})

	grpcServerCurrentState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "grpc_server_current_state",
		Help: "Current state of the gRPC server's subchannel. 0: UNKNOWN; 1: IDLE; 2: CONNECTING; 3: READY; 4: TRANSIENT_FAILURE; 5: SHUTDOWN",
	}, []string{"target"})
)

type LocalMetricClient struct {
	grpc_channelz_v1.ChannelzClient
	addr string
}

func (l *LocalMetricClient) getAddr() string {
	return l.addr
}

func bindMetrics() error {
	// grpc metrics
	g := []prometheus.Collector{
		grpcServerCallsFailedTotal,
		grpcServerCallsStartedTotal,
		grpcServerLastCallStartedSeconds,
		grpcServerCurrentState,
	}
	for _, c := range g {
		if err := ClientMetrics.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// CreateChannelzMonitor creates a localhost channelz server and a `metricClient` for it.
func CreateChannelzMonitor() (*LocalMetricClient, error) {
	// Channelz monitoring works by having a local GRPC server responding to Channelz queries using GRPC.
	metricServer := grpc.NewServer()
	service.RegisterChannelzServiceToServer(metricServer)
	//	If the port in the address parameter is empty or "0", as in
	// "127.0.0.1:" or "[::1]:0", a port number is automatically chosen
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		slog.Error("failed to listen on grpc metrics", "err", err)
		return nil, err
	}

	go func() {
		if err := metricServer.Serve(lis); err != nil {
			slog.Error("error serving grpc metrics", "err", err)
		}
	}()

	// create our global metric client
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("Error building channelz client, shutting down server now", "err", err)
		metricServer.GracefulStop()
		return nil, err
	}
	return &LocalMetricClient{
		grpc_channelz_v1.NewChannelzClient(cc),
		lis.Addr().String(),
	}, nil
}

func UpdateMetrics(metricClient *LocalMetricClient) string {
	// we need to get all root channels
	resp, err := metricClient.GetTopChannels(context.Background(), &grpc_channelz_v1.GetTopChannelsRequest{})
	if err != nil {
		slog.Error("Error GetTopChannels", "err", err)
		return "Error GetTopChannels"
	}

	ret := make([]*grpc_channelz_v1.GetSubchannelResponse, 0)
	for _, respCh := range resp.GetChannel() {
		for _, sc := range respCh.GetSubchannelRef() {
			subr, err := metricClient.GetSubchannel(context.Background(), &grpc_channelz_v1.GetSubchannelRequest{SubchannelId: sc.GetSubchannelId()})
			if err != nil {
				slog.Error("Error GetChannel", "err", err)
				return "Error GetChannel"
			}
			ret = append(ret, subr)

			target := subr.GetSubchannel().GetData().GetTarget()
			if target == metricClient.getAddr() {
				// we don't need metrics about our local metric server and metric client
				continue
			}

			grpcServerCallsFailedTotal.With(prometheus.Labels{
				"target": target,
			}).Set(float64(subr.GetSubchannel().GetData().GetCallsFailed()))

			grpcServerCallsStartedTotal.With(prometheus.Labels{
				"target": target,
			}).Set(float64(subr.GetSubchannel().GetData().GetCallsStarted()))

			grpcServerLastCallStartedSeconds.With(prometheus.Labels{
				"target": target,
			}).Set(float64(subr.GetSubchannel().GetData().GetLastCallStartedTimestamp().GetSeconds()))

			currentState := subr.GetSubchannel().GetData().GetState()
			grpcServerCurrentState.With(prometheus.Labels{
				"target": target,
			}).Set(float64(currentState.GetState()))
		}
	}

	return ToJSON(ret)
}
