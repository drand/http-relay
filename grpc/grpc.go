package grpc

import (
	"context"
	"log"

	protocommon "github.com/drand/drand/protobuf/common"
	proto "github.com/drand/drand/protobuf/drand"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn       *grpc.ClientConn
	serverAddr string
}

func NewClient(serverAddr string) (*Client, error) {
	conn, err := grpc.Dial(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %s", err)
	}

	return &Client{conn: conn, serverAddr: serverAddr}, nil
}

type RandomData interface {
	GetRound() uint64
	GetRandomness() []byte
	GetSignature() []byte
	GetPreviousSignature() []byte
}

func (c *Client) GetBeacon(chainId string, round uint64) (RandomData, error) {
	client := proto.NewPublicClient(c.conn)
	in := &proto.PublicRandRequest{
		Round:    round,
		Metadata: &protocommon.Metadata{BeaconID: chainId},
	}
	randResp, err := client.PublicRand(context.Background(), in)
	if err != nil {
		return nil, err
	}

	return randResp, nil
}

func (c *Client) Health(chainId string) (bool, error) {
	client := proto.NewControlClient(c.conn)
	in := &proto.StatusRequest{
		CheckConn: []*proto.Address{
			{Address: c.serverAddr},
		},
		Metadata: &protocommon.Metadata{BeaconID: chainId},
	}
	resp, err := client.Status(context.Background(), in)
	if err != nil {
		return false, err
	}

	if cs := resp.GetChainStore(); cs != nil {
		return !cs.IsEmpty && cs.GetLastRound() == cs.GetLength(), nil
	}

	return false, nil
}

type ChainInfo interface {
	GetPublicKey() []byte
	GetPeriod() uint32
	GetGenesisTime() int64
	GetHash() []byte
	GetGroupHash() []byte
	GetSchemeID() string
}

func (c *Client) GetChainInfo(chainId string) (ChainInfo, error) {
	client := proto.NewPublicClient(c.conn)
	in := &proto.ChainInfoRequest{
		Metadata: &protocommon.Metadata{BeaconID: chainId},
	}
	resp, err := client.ChainInfo(context.Background(), in)
	if err != nil {
		return nil, err
	}

	return resp, err
}
