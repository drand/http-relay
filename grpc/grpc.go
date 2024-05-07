package grpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/timeout"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/protoadapt"
)

func init() {
	balancer.Register(NewFallbackBuilder(3 * time.Second))
	// registers the logging_pick_first_with_fallback balancer
	balancer.Register(NewLoggingBalancerBuilder("pick_first_with_fallback", slog.With("service", "balancer")))
	bindMetrics()
}

type logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
	Debug(msg string, args ...any)
}

// Client represent a drand GRPC client, it connects to a single node at serverAddr, stores the connection in conn
// it has a knownChains map of known chain info keyed using the hex-encoded chainhash of a beacon chain. The checkTimeout
// is used for health checks only currently.
type Client struct {
	conn         *grpc.ClientConn
	pc           proto.PublicClient
	serverAddr   string
	knownChains  sync.Map
	checkTimeout time.Duration
	log          logger
}

// NewClient establishes a new non-TLS grpc connection to the provided server address. It takes a logger and uses
// a default value for checkTimeout.
func NewClient(serverAddr string, l logger) (*Client, error) {
	l.Debug("NewClient", "serverAddr", serverAddr)

	// setup metrics for GRPC calls
	clMetrics := grpcprom.NewClientMetrics(
		grpcprom.WithClientHandlingTimeHistogram(
			// Based on our current AWS same region latency:
			//  P50       P75      P90       P95      P99      P99.9     P99.99
			//  1.094ms  6.62ms  17.886ms  25.78ms  48.124ms  81.533ms  123.466ms
			// with extra long buckets to try and catch the connections that are "too early"
			grpcprom.WithHistogramBuckets([]float64{.002, .007, .02, .05, .125, .5, 1, 2, 5, 10, 25}),
		),
	)

	// register client metrics
	ClientMetrics.Register(clMetrics)

	conn, err := grpc.Dial(serverAddr,
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"logging_pick_first_with_fallback"}`),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(
			// TODO: do we really want a 500ms default checkTimeout on unary RPC?
			timeout.UnaryClientInterceptor(500*time.Millisecond),
			clMetrics.UnaryClientInterceptor(),
			UsedEndpointInterceptor(l),
		),
		grpc.WithChainStreamInterceptor(
			clMetrics.StreamClientInterceptor(),
		),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		l.Error("Unable to Dial", "err", err)
	}
	client := &Client{
		conn:         conn,
		pc:           proto.NewPublicClient(conn),
		serverAddr:   serverAddr,
		checkTimeout: 3 * time.Second,
		log:          l,
	}

	// we do a GetChains call to pre-populate the knownChains, note that we have a 500ms checkTimeout built-in above
	_, err = client.GetChains(context.Background())
	return client, err
}

// SetTimeout allows to set your own checkTimeout for GRPC health checks.
func (c *Client) SetTimeout(timeout time.Duration) {
	c.log.Debug("Client SetTimeout")

	c.checkTimeout = timeout
}

func (c *Client) Close() error {
	c.log.Debug("Client Closing")
	return c.conn.Close()
}

// GetBeacon will fetch the requested beacon. Beacons starts at 1, asking for 0 provides the latest, asking for
// the next one will most likely cause the server to wait until it's produced to send it your way.
func (c *Client) GetBeacon(ctx context.Context, m *proto.Metadata, round uint64) (*HexBeacon, error) {
	c.log.Debug("Client GetBeacon", "round", round)

	in := &proto.PublicRandRequest{
		Round:    round,
		Metadata: m,
	}

	randResp, err := c.pc.PublicRand(ctx, in)
	if err != nil {
		c.log.Debug("GetBeacon failed once")
		// we do 1 retry (automagically with the next subconn thanks to the fallback LB) if it failed
		randResp, err = c.pc.PublicRand(ctx, in)
		if err != nil {
			return nil, err
		}
	}

	return NewHexBeacon(randResp), nil
}

// Watch returns new randomness as it becomes available.
func (c *Client) Watch(ctx context.Context, m *proto.Metadata) <-chan *HexBeacon {
	c.log.Debug("Client Watch")
	stream, err := c.pc.PublicRandStream(ctx, &proto.PublicRandRequest{Round: 0, Metadata: m})
	ch := make(chan *HexBeacon, 1)
	if err != nil {
		close(ch)
		return nil
	}
	go func() {
		defer close(ch)
		for {
			next, err := stream.Recv()
			switch {
			case err != nil:
				c.log.Error("public rand stream error", "err", err)
				return
			case stream.Context().Err() != nil:
				c.log.Error("public rand stream Ctx error", "err", stream.Context().Err())
				return
			case ctx.Err() != nil:
				c.log.Error("watch outer Ctx error", "err", stream.Context().Err())
				return
			}
			ch <- NewHexBeacon(next)
		}
	}()
	return ch
}

// Check is relying on GRPC default health reporting service, it does not indicate whether a node is behind or not,
// only whether a node is currently up or not.
func (c *Client) Check(ctx context.Context) error {
	c.log.Debug("Client Check")

	client := healthgrpc.NewHealthClient(c.conn)

	tctx, cancel := context.WithTimeout(ctx, c.checkTimeout)
	defer cancel()

	resp, err := client.Check(tctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		// we do 1 retry (automagically with the next subconn thanks to the fallback LB) if it failed
		resp, err = client.Check(ctx, &healthgrpc.HealthCheckRequest{})
		if err != nil {
			return err
		}
	}

	if resp.GetStatus() != healthgrpc.HealthCheckResponse_SERVING {
		return fmt.Errorf("grpc health: not serving")
	}

	return nil
}

// JsonInfoV1 is the V1 representation of the chain info.
type JsonInfoV1 struct {
	PublicKey   HexBytes        `json:"public_key"`
	Period      uint32          `json:"period"`
	GenesisTime int64           `json:"genesis_time"`
	Hash        HexBytes        `json:"hash"`
	GroupHash   HexBytes        `json:"groupHash"`
	SchemeID    string          `json:"schemeID,omitempty"`
	Metadata    *proto.Metadata `json:"metadata,omitempty"`
}

// JsonInfoV2 is the V2 representation of the chain info, which contains breaking changes compared to V1.
type JsonInfoV2 struct {
	PublicKey   HexBytes `json:"public_key"`
	Period      uint32   `json:"period"`
	GenesisTime int64    `json:"genesis_time"`
	GenesisSeed HexBytes `json:"genesis_seed,omitempty"`
	Hash        HexBytes `json:"chain_hash"`
	Scheme      string   `json:"scheme"`
	BeaconId    string   `json:"beacon_id"`
}

func NewInfoV2(resp *proto.ChainInfoPacket) *JsonInfoV2 {
	return &JsonInfoV2{
		PublicKey:   resp.GetPublicKey(),
		BeaconId:    resp.GetMetadata().GetBeaconID(),
		Period:      resp.GetPeriod(),
		Scheme:      resp.GetSchemeID(),
		GenesisTime: resp.GetGenesisTime(),
		GenesisSeed: resp.GetGroupHash(),
		Hash:        resp.GetMetadata().GetChainHash(),
	}
}

func (info *JsonInfoV2) ExpectedNext() (expectedTime int64, expectedRound uint64) {
	p := int64(info.Period)
	// we rely on integer division rounding down, plus one because round 1 happened at GenesisTime
	expected := ((time.Now().Unix() - info.GenesisTime) / p) + 1
	return expected*p + info.GenesisTime, uint64(expected)
}

func (j *JsonInfoV2) V1() *JsonInfoV1 {
	return &JsonInfoV1{
		PublicKey:   j.PublicKey,
		Period:      j.Period,
		GenesisTime: j.GenesisTime,
		Hash:        j.Hash,
		GroupHash:   j.GenesisSeed,
		SchemeID:    j.Scheme,
		Metadata:    &proto.Metadata{BeaconID: j.BeaconId},
	}
}

func (j *JsonInfoV1) V2() *JsonInfoV2 {
	return &JsonInfoV2{
		PublicKey:   j.PublicKey,
		Period:      j.Period,
		GenesisTime: j.GenesisTime,
		Hash:        j.Hash,
		GenesisSeed: j.GroupHash,
		BeaconId:    j.Metadata.GetBeaconID(),
		Scheme:      j.SchemeID,
	}
}

// GetChainInfo returns the chain info for the requested chainhash or beacon ID in the provided Metadata, the Metadata
// should specify either a beacon ID or a chain hash, not both in order to benefit from in chain info caching.
func (c *Client) GetChainInfo(ctx context.Context, m *proto.Metadata) (*JsonInfoV2, error) {
	c.log.Debug("Client GetChainInfo")

	// typically either chain hash or beacon id are set, not both, unless the API is misused
	if info, ok := c.knownChains.Load(hex.EncodeToString(m.GetChainHash()) + m.GetBeaconID()); ok {
		res, ok := info.(*JsonInfoV2)
		if ok {
			return res, nil
		}
		c.log.Error("Client GetChainInfo: unexpected non-JsonInfoV2 content in map", "res", res)
	}

	c.log.Debug("Client GetChainInfo knownChains", "cache", "MISS")

	in := &proto.ChainInfoRequest{
		Metadata: m,
	}

	resp, err := c.pc.ChainInfo(ctx, in)
	if err != nil {
		return nil, err
	}

	info := NewInfoV2(resp)
	c.knownChains.Store(info.Hash.String(), info)

	// we also have a shortcut for handling beacon IDs, which relies on the fact that we expect either chain hash
	// or beacon ID in metadata, not both.
	c.knownChains.Store(info.BeaconId, info)

	return info, err
}

// GetChains returns an array of chain-hashes available on that grpc node. It does 1 ListBeaconIDs call and n calls to
// get the ChainInfo, so it's a relatively noisy path.
func (c *Client) GetChains(ctx context.Context) ([]string, error) {
	c.log.Debug("Client GetChains")

	resp, err := c.pc.ListBeaconIDs(ctx, &proto.ListBeaconIDsRequest{})
	if err != nil {
		c.log.Error("client.ListBeaconIDs", "err", err)
		return nil, err
	}

	beaconIds := resp.GetIds()
	metadatas := resp.GetMetadatas()

	if len(beaconIds) != len(metadatas) {
		return nil, fmt.Errorf("invalid response: received %d beacon IDs (%v) but %d metadata packets", len(beaconIds), beaconIds, len(metadatas))
	}

	chains := make([]string, 0, len(metadatas))
	for _, meta := range metadatas {
		chain := meta.GetChainHash()
		strChain := hex.EncodeToString(chain)
		chains = append(chains, strChain)
		_, ok := c.knownChains.Load(strChain)
		if ok {
			continue
		}

		in := &proto.ChainInfoRequest{
			Metadata: &proto.Metadata{ChainHash: chain},
		}

		info, err := c.pc.ChainInfo(ctx, in)
		if err != nil {
			c.log.Error("invalid call to ChainInfo", "err", err)
			return nil, err
		}
		hash := info.GetHash()
		if !bytes.Equal(chain, hash) {
			return nil, fmt.Errorf("invalid chainhash %q for chain %q", hash, chain)
		}
		c.knownChains.Store(strChain, NewInfoV2(info))
		if id := info.GetMetadata().GetBeaconID(); id != "" {
			c.knownChains.Store(id, NewInfoV2(info))
		}
	}

	return chains, err
}

func (c *Client) String() string {
	return c.serverAddr
}

const jsonIndent = "  "

// ToJSON marshals the input into a json string.
//
// If marshal fails, it falls back to fmt.Sprintf("%+v").
func ToJSON(e any) string {
	if ee, ok := e.(protoadapt.MessageV1); ok {
		e = protoadapt.MessageV2Of(ee)
	}

	if ee, ok := e.(protoadapt.MessageV2); ok {
		mm := protojson.MarshalOptions{
			Indent:    jsonIndent,
			Multiline: true,
		}
		ret, err := mm.Marshal(ee)
		if err != nil {
			// This may fail for proto.Anys, e.g. for xDS v2, LDS, the v2
			// messages are not imported, and this will fail because the message
			// is not found.
			return fmt.Sprintf("%+v", ee)
		}
		return string(ret)
	}

	ret, err := json.MarshalIndent(e, "", jsonIndent)
	if err != nil {
		return fmt.Sprintf("%+v", e)
	}
	return string(ret)
}

// FormatJSON formats the input json bytes with indentation.
//
// If Indent fails, it returns the unchanged input as string.
func FormatJSON(b []byte) string {
	var out bytes.Buffer
	err := json.Indent(&out, b, "", jsonIndent)
	if err != nil {
		return string(b)
	}
	return out.String()
}
