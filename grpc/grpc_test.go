package grpc

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNextBeaconTime(t *testing.T) {
	clock = func() time.Time {
		return time.Unix(1718551765, 0)
	}

	tests := []struct {
		name          string
		info          *JsonInfoV2
		expectedTime  int64
		expectedRound uint64
	}{
		{
			"first",
			&JsonInfoV2{
				Period:      10,
				GenesisTime: clock().Unix() - 25,
			},
			clock().Unix() + 5,
			4,
		},
		{
			"second",
			&JsonInfoV2{
				Period:      1,
				GenesisTime: clock().Unix() - 3,
			},
			clock().Unix() + 1,
			5,
		},
		{
			"mainnet-default",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: 1595431050,
			},
			1718551770,
			4104025,
		},
		{
			"now",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: clock().Unix(),
			},
			clock().Unix() + 30,
			2,
		},
		{
			"now-33",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: clock().Unix() - 33,
			},
			clock().Unix() + 27,
			3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotr := tt.info.ExpectedNext()
			if got != tt.expectedTime {
				t.Errorf("unexpect next time: got = %v, want %v", got, tt.expectedTime)
			}
			if gotr != tt.expectedRound {
				t.Errorf("%s: unexpected next round: got = %v, want %v", tt.name, gotr, tt.expectedRound)
			}
		})
	}
}

func TestNewClient_InvalidAddress(t *testing.T) {
	l := slog.Default()

	_, err := NewClient("invalid://address that will fail", l)
	if err == nil {
		// gRPC uses lazy connections, so NewClient may not fail directly
		t.Log("no error on creation (lazy conn); skipping")
		return
	}

	if !strings.Contains(err.Error(), "unable to create grpc client") &&
		!strings.Contains(err.Error(), "error") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCheck_CancelledContext(t *testing.T) {
	l := slog.Default()

	// connect to a non-existent server; conn is lazy so NewClient won't fail
	client, err := NewClient("localhost:0", l)
	if err != nil {
		t.Skipf("NewClient failed: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = client.Check(ctx)
	if err == nil {
		t.Error("expected error with cancelled context, got nil")
	}
}
