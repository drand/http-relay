package grpc

import (
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

func TestSecondsUntilRound(t *testing.T) {
	now := int64(1718551765)
	clock = func() time.Time {
		return time.Unix(now, 0)
	}

	tests := []struct {
		name     string
		info     *JsonInfoV2
		round    uint64
		wantSecs int64
	}{
		{
			name: "next round in 5 seconds",
			info: &JsonInfoV2{
				Period:      10,
				GenesisTime: now - 25,
			},
			round:    4,
			wantSecs: 5,
		},
		{
			name: "round 0 returns 0",
			info: &JsonInfoV2{
				Period:      10,
				GenesisTime: now - 25,
			},
			round:    0,
			wantSecs: 0,
		},
		{
			name: "round already passed returns 0",
			info: &JsonInfoV2{
				Period:      10,
				GenesisTime: now - 100,
			},
			round:    5,
			wantSecs: 0,
		},
		{
			name: "round in 30 seconds",
			info: &JsonInfoV2{
				Period:      30,
				GenesisTime: now,
			},
			round:    2,
			wantSecs: 30,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.info.SecondsUntilRound(tt.round)
			if got != tt.wantSecs {
				t.Errorf("SecondsUntilRound() = %v, want %v", got, tt.wantSecs)
			}
		})
	}
}
