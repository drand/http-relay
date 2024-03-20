package grpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	proto "github.com/drand/drand/v2/protobuf/drand"
)

type logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
	Debug(msg string, args ...any)
}

// Client represent a drand GRPC client, it connects to a single node at serverAddr, stores the connection in conn
// it has a knownChains map of known chain info keyed using the hex-encoded chainhash of a beacon chain. The timeout
// is used for health checks only currently.
type Client struct {
	conn        *grpc.ClientConn
	serverAddr  string
	knownChains sync.Map
	timeout     time.Duration
	log         logger
}

// NewClient establishes a new non-TLS grpc connection to the provided server address. It takes a logger and uses
// a default value for timeout.
func NewClient(serverAddr string, l logger) (*Client, error) {
	l.Debug("NewClient", "serverAddr", serverAddr)

	conn, err := grpc.Dial(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		l.Error("Unable to Dial", "err", err)
	}
	client := &Client{
		conn:       conn,
		serverAddr: serverAddr,
		timeout:    3 * time.Second,
		log:        l,
	}
	// we do a GetChains call to pre-populate the knownChains with a special timeout of 1 minute
	ctx, cancel := context.WithTimeout(context.Background(), client.timeout)
	defer cancel()
	_, err = client.GetChains(ctx)
	return client, err
}

// SetTimeout allows to set your own timeout for GRPC health checks.
func (c *Client) SetTimeout(timeout time.Duration) {
	c.log.Debug("Client SetTimeout")

	c.timeout = timeout
}

func (c *Client) Close() error {
	c.log.Debug("Client Closing")
	return c.conn.Close()
}

// GetBeacon will fetch the requested beacon. Beacons starts at 1, asking for 0 provides the latest, asking for
// the next one will most likely cause the server to wait until it's produced to send it your way.
func (c *Client) GetBeacon(ctx context.Context, m *proto.Metadata, round uint64) (*HexBeacon, error) {
	c.log.Debug("Client GetBeacon", "round", round)

	client := proto.NewPublicClient(c.conn)

	in := &proto.PublicRandRequest{
		Round:    round,
		Metadata: m,
	}

	randResp, err := client.PublicRand(ctx, in)
	if err != nil {
		return nil, err
	}

	return NewHexBeacon(randResp), nil
}

// Watch returns new randomness as it becomes available.
func (c *Client) Watch(ctx context.Context, m *proto.Metadata) <-chan *HexBeacon {
	c.log.Debug("Client Watch")
	client := proto.NewPublicClient(c.conn)
	stream, err := client.PublicRandStream(ctx, &proto.PublicRandRequest{Round: 0, Metadata: m})
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

// Check is returning the latest beacon and the chain info for the requested chainhash or beacon ID in the provided Metadata
func (c *Client) Check(ctx context.Context) error {
	c.log.Debug("Client Check")

	client := healthgrpc.NewHealthClient(c.conn)

	tctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := client.Check(tctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		return err
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

// GetChainInfo returns the chain info for the requested chainhash or beacon ID in the provided Metadata
func (c *Client) GetChainInfo(ctx context.Context, m *proto.Metadata) (*JsonInfoV2, error) {
	c.log.Debug("Client GetChainInfo")

	if info, ok := c.knownChains.Load(hex.EncodeToString(m.GetChainHash())); ok {
		res, ok := info.(*JsonInfoV2)
		if ok {
			c.log.Debug("Client GetChainInfo knownChains", "cache", "HIT")
			return res, nil
		}
		c.log.Error("Client GetChainInfo: unexpected non-JsonInfoV2 content in map", "res", res)
	}

	c.log.Debug("Client GetChainInfo knownChains", "cache", "MISS")

	client := proto.NewPublicClient(c.conn)

	in := &proto.ChainInfoRequest{
		Metadata: m,
	}

	resp, err := client.ChainInfo(ctx, in)
	if err != nil {
		return nil, err
	}

	info := NewInfoV2(resp)
	c.knownChains.Store(info.Hash.String(), info)

	return info, err
}

// GetChains returns an array of chain-hashes available on that grpc node. It does 1 ListBeaconIDs call and n calls to
// get the ChainInfo, so it's a relatively noisy path.
func (c *Client) GetChains(ctx context.Context) ([]string, error) {
	c.log.Debug("Client GetChains")

	client := proto.NewPublicClient(c.conn)
	resp, err := client.ListBeaconIDs(ctx, &proto.ListBeaconIDsRequest{})
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

		info, err := client.ChainInfo(ctx, in)
		if err != nil {
			c.log.Error("invalid call to ChainInfo", "err", err)
			return nil, err
		}
		hash := info.GetHash()
		if !bytes.Equal(chain, hash) {
			return nil, fmt.Errorf("invalid chainhash %q for chain %q", hash, chain)
		}
		c.knownChains.Store(strChain, NewInfoV2(info))
	}

	return chains, err
}
