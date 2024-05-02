package grpc

import (
	"context"
	"log"
	"log/slog"
	"net"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/channelz/grpc_channelz_v1"
	"google.golang.org/grpc/channelz/service"
	"google.golang.org/grpc/credentials/insecure"
)

var (
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
		Help: "Current state of the gRPC server's subchannel.",
	}, []string{"target"})
)

func RegisterMetrics(r prometheus.Registerer) error {
	// grpc metrics
	g := []prometheus.Collector{
		grpcServerCallsFailedTotal,
		grpcServerCallsStartedTotal,
		grpcServerLastCallStartedSeconds,
		grpcServerCurrentState,
	}
	for _, c := range g {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// spin out a channelz server
func startMonitoringServer(addr string) {
	metricServer := grpc.NewServer()
	service.RegisterChannelzServiceToServer(metricServer)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	if err := metricServer.Serve(lis); err != nil {
		slog.Error("error listening on metric server", "err", err)
		log.Fatalf("failed to serve: %v", err)
	}
}

func UpdateMetrics() string {
	cc, err := grpc.Dial("127.0.0.1:7555", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("Error building channelz client", "err", err)
		return "Error building channelz client"
	}
	client := grpc_channelz_v1.NewChannelzClient(cc)
	resp, err := client.GetTopChannels(context.Background(), &grpc_channelz_v1.GetTopChannelsRequest{})
	if err != nil {
		slog.Error("Error GetTopChannels", "err", err)
		return "Error GetTopChannels"
	}

	var strRet string

	var r *grpc_channelz_v1.GetChannelResponse
	for _, ch := range resp.GetChannel() {
		if strings.HasPrefix(ch.Ref.GetName(), "fallback") {
			strRet += ToJSON(ch)
			r, err = client.GetChannel(context.Background(), &grpc_channelz_v1.GetChannelRequest{ChannelId: ch.Ref.GetChannelId()})
			if err != nil {
				slog.Error("Error GetChannel", "err", err)
				return "Error GetChannel"
			}
			break
		}
	}

	strRet += ToJSON(r)

	subChannels := r.GetChannel().GetSubchannelRef()
	ret := make([]*grpc_channelz_v1.GetSubchannelResponse, 0, len(subChannels))
	for _, sc := range subChannels {
		subr, err := client.GetSubchannel(context.Background(), &grpc_channelz_v1.GetSubchannelRequest{SubchannelId: sc.GetSubchannelId()})
		if err != nil {
			slog.Error("Error GetChannel", "err", err)
			return "Error GetChannel"
		}
		ret = append(ret, subr)
		slog.Warn("YOLO", "Target", subr.GetSubchannel().GetData().GetTarget(),
			"GetCallsFailed", subr.GetSubchannel().GetData().GetCallsFailed(),
			"GetCallsStarted", subr.GetSubchannel().GetData().GetCallsStarted(),
			"GetLastCallStartedTimestampSeconds", subr.GetSubchannel().GetData().GetLastCallStartedTimestamp().GetSeconds(),
			"currentState", subr.GetSubchannel().GetData().GetState(),
		)

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

	return strRet + ToJSON(ret)
}
