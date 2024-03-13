package grpc

import (
	"context"
	"encoding/hex"
	"log"
	"sync"

	proto "github.com/drand/drand/protobuf/drand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn           *grpc.ClientConn
	serverAddr     string
	knownBeaconIds sync.Map
}

func NewClient(serverAddr string) (*Client, error) {
	conn, err := grpc.Dial(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %s", err)
	}

	return &Client{conn: conn, serverAddr: serverAddr}, nil
}

// GetBeacon will fetch the requested beacon. Beacons starts at 1, asking for 0 provides the latest, asking for
// the next one will most likely cause the server to wait until it's produced to send it your way.
func (c *Client) GetBeacon(ctx context.Context, m *proto.Metadata, round uint64) (*HexBeacon, error) {
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

// GetHealth is returning the latest beacon and the chain info for the requested chainhash or beacon ID in the provided Metadata
func (c *Client) GetHealth(ctx context.Context, m *proto.Metadata) (uint64, *proto.ChainInfoPacket, error) {
	client := proto.NewPublicClient(c.conn)

	latest := &proto.PublicRandRequest{
		Round:    0,
		Metadata: m,
	}

	randResp, err := client.PublicRand(ctx, latest)
	if err != nil {
		return 0, nil, err
	}

	infoReq := &proto.ChainInfoRequest{
		Metadata: m,
	}

	info, err := client.ChainInfo(ctx, infoReq)
	if err != nil {
		return 0, nil, err
	}

	return randResp.GetRound(), info, nil
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
	client := proto.NewPublicClient(c.conn)

	in := &proto.ChainInfoRequest{
		Metadata: m,
	}

	resp, err := client.ChainInfo(ctx, in)
	if err != nil {
		return nil, err
	}

	info := &JsonInfoV2{
		PublicKey:   resp.GetPublicKey(),
		BeaconId:    resp.GetMetadata().GetBeaconID(),
		Period:      resp.GetPeriod(),
		Scheme:      resp.GetSchemeID(),
		GenesisTime: resp.GetGenesisTime(),
		GenesisSeed: resp.GetGroupHash(),
		Hash:        resp.GetMetadata().GetChainHash(),
	}

	return info, err
}

// GetChains returns an array of chainhashes available on that grpc node. It does 1 ListBeaconIDs call and n calls to
// get the ChainInfo, so it's a relatively noisy path.
func (c *Client) GetChains(ctx context.Context) ([]string, error) {
	client := proto.NewPublicClient(c.conn)
	resp, err := client.ListBeaconIDs(ctx, &proto.ListBeaconIDsRequest{})
	if err != nil {
		return nil, err
	}

	chains := make([]string, 0, len(resp.Ids))
	for _, beaconID := range resp.Ids {
		in := &proto.ChainInfoRequest{
			Metadata: &proto.Metadata{BeaconID: beaconID},
		}

		info, err := client.ChainInfo(ctx, in)
		if err != nil {
			return nil, err
		}
		chainhash := hex.EncodeToString(info.GetHash())
		chains = append(chains, chainhash)
	}

	return chains, err
}
