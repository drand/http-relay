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
func (c *Client) GetBeacon(m *proto.Metadata, round uint64) (*HexBeacon, error) {
	client := proto.NewPublicClient(c.conn)

	in := &proto.PublicRandRequest{
		Round:    round,
		Metadata: m,
	}

	randResp, err := client.PublicRand(context.Background(), in)
	if err != nil {
		return nil, err
	}

	return NewHexBeacon(randResp), nil
}

func (c *Client) GetHealth(m *proto.Metadata) (uint64, *proto.ChainInfoPacket, error) {
	client := proto.NewPublicClient(c.conn)

	latest := &proto.PublicRandRequest{
		Round:    0,
		Metadata: m,
	}

	randResp, err := client.PublicRand(context.Background(), latest)
	if err != nil {
		return 0, nil, err
	}

	infoReq := &proto.ChainInfoRequest{
		Metadata: m,
	}

	info, err := client.ChainInfo(context.Background(), infoReq)
	if err != nil {
		return 0, nil, err
	}

	return randResp.GetRound(), info, nil
}

// NB. this is a breaking change to our API, since renamed a few fields
type JsonInfo struct {
	PublicKey   HexBytes        `json:"public_key"`
	Period      uint32          `json:"period"`
	GenesisTime int64           `json:"genesis_time"`
	GenesisSeed HexBytes        `json:"genesis_seed,omitempty"`
	Hash        HexBytes        `json:"hash"`
	GroupHash   HexBytes        `json:"groupHash,omitempty"`
	BeaconId    string          `json:"beacon_id"`
	Scheme      string          `json:"scheme,omitempty"`
	SchemeID    string          `json:"schemeID,omitempty"`
	Metadata    *proto.Metadata `json:"metadata,omitempty"`
}

func (j *JsonInfo) V1() *JsonInfo {
	return &JsonInfo{
		PublicKey:   j.PublicKey,
		Period:      j.Period,
		GenesisTime: j.GenesisTime,
		Hash:        j.Hash,
		GenesisSeed: nil,
		GroupHash:   j.GenesisSeed,
		BeaconId:    "",
		Scheme:      "",
		SchemeID:    j.Scheme,
		Metadata:    &proto.Metadata{BeaconID: j.BeaconId},
	}
}

func (j *JsonInfo) V2() *JsonInfo {
	return &JsonInfo{
		PublicKey:   j.PublicKey,
		Period:      j.Period,
		GenesisTime: j.GenesisTime,
		Hash:        j.Hash,
		GenesisSeed: j.GenesisSeed,
		GroupHash:   nil,
		BeaconId:    j.BeaconId,
		Scheme:      j.Scheme,
		SchemeID:    "",
		Metadata:    nil,
	}
}

// GetChainInfo returns the chain info for the requested chainhash
func (c *Client) GetChainInfo(m *proto.Metadata) (*JsonInfo, error) {
	client := proto.NewPublicClient(c.conn)

	in := &proto.ChainInfoRequest{
		Metadata: m,
	}

	resp, err := client.ChainInfo(context.Background(), in)
	if err != nil {
		return nil, err
	}

	info := &JsonInfo{
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
func (c *Client) GetChains() ([]string, error) {
	client := proto.NewPublicClient(c.conn)
	resp, err := client.ListBeaconIDs(context.Background(), &proto.ListBeaconIDsRequest{})
	if err != nil {
		return nil, err
	}

	chains := make([]string, 0, len(resp.Ids))
	for _, beaconID := range resp.Ids {
		in := &proto.ChainInfoRequest{
			Metadata: &proto.Metadata{BeaconID: beaconID},
		}

		info, err := client.ChainInfo(context.Background(), in)
		if err != nil {
			return nil, err
		}
		chainhash := hex.EncodeToString(info.GetHash())
		chains = append(chains, chainhash)
	}

	return chains, err
}
