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

	metricClient grpc_channelz_v1.ChannelzClient
)

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

// spin out a channelz server
func startMonitoringServer(addr string) error {
	metricServer := grpc.NewServer()
	service.RegisterChannelzServiceToServer(metricServer)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("failed to listen on grpc metrics", "err", err)
		return err
	}

	go func() {
		if err := metricServer.Serve(lis); err != nil {
			slog.Error("error serving grpc metrics", "err", err)
		}
	}()

	// create our global metric client
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("Error building channelz client, shutting down server now", "err", err)
		metricServer.GracefulStop()
		return err
	}
	metricClient = grpc_channelz_v1.NewChannelzClient(cc)
	return nil
}

func UpdateMetrics() string {

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

			grpcServerCallsFailedTotal.With(prometheus.Labels{
				"target": subr.GetSubchannel().GetData().GetTarget(),
			}).Set(float64(subr.GetSubchannel().GetData().GetCallsFailed()))

			grpcServerCallsStartedTotal.With(prometheus.Labels{
				"target": subr.GetSubchannel().GetData().GetTarget(),
			}).Set(float64(subr.GetSubchannel().GetData().GetCallsStarted()))

			grpcServerLastCallStartedSeconds.With(prometheus.Labels{
				"target": subr.GetSubchannel().GetData().GetTarget(),
			}).Set(float64(subr.GetSubchannel().GetData().GetLastCallStartedTimestamp().GetSeconds()))

			currentState := subr.GetSubchannel().GetData().GetState()
			grpcServerCurrentState.With(prometheus.Labels{
				"target": subr.GetSubchannel().GetData().GetTarget(),
			}).Set(float64(currentState.GetState())) // Just to indicate that the state is this value
		}
	}

	return ToJSON(ret)
}
