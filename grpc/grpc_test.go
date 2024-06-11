package grpc

import (
	"testing"
	"time"
)

func TestNextBeaconTime(t *testing.T) {
	clock = func() time.Time {
		return time.Unix(1718110233, 0)
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
				PublicKey:   HexBytes{00, 01, 02, 03, 04},
				Period:      10,
				GenesisTime: clock().Unix() - 25,
				GenesisSeed: []byte("test"),
				Hash:        []byte("test"),
				Scheme:      "test",
				BeaconId:    "default",
			},
			clock().Unix() + 5,
			3,
		},
		{
			"second",
			&JsonInfoV2{
				PublicKey:   HexBytes{00, 01, 02, 03, 04},
				Period:      13,
				GenesisTime: clock().Unix() - 33,
				GenesisSeed: []byte("test"),
				Hash:        []byte("test"),
				Scheme:      "test",
				BeaconId:    "default",
			},
			clock().Unix() + 6,
			3,
		},
		{
			"mainnet-default",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: 1595431050,
			},
			1718110260,
			4089308,
		},
		{
			"now",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: 1718110233,
			},
			1718110263,
			1,
		},
		{
			"now",
			&JsonInfoV2{
				Period:      30,
				GenesisTime: 1718110200,
			},
			1718110260,
			2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotr := tt.info.ExpectedNext()
			if got != tt.expectedTime {
				t.Errorf("unexpect next time: got = %v, want %v", got, tt.expectedTime)
			}
			if gotr != tt.expectedRound {
				t.Errorf("unexpected next round: got = %v, want %v", gotr, tt.expectedRound)
			}
		})
	}
}
