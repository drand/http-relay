package grpc

import (
	"testing"
	"time"
)

func TestNextBeaconTime(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name         string
		info         *JsonInfoV2
		expectedTime int64
	}{
		{
			"default",
			&JsonInfoV2{
				PublicKey:   HexBytes{00, 01, 02, 03, 04},
				Period:      10,
				GenesisTime: now - 25,
				GenesisSeed: []byte("test"),
				Hash:        []byte("test"),
				Scheme:      "test",
				BeaconId:    "default",
			},
			now + 5,
		},
		{
			"quicknet",
			&JsonInfoV2{
				PublicKey:   HexBytes{00, 01, 02, 03, 04},
				Period:      13,
				GenesisTime: now - 33,
				GenesisSeed: []byte("test"),
				Hash:        []byte("test"),
				Scheme:      "test",
				BeaconId:    "default",
			},
			now + 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := tt.info.ExpectedNext()
			if got != tt.expectedTime {
				t.Errorf("NextBeaconTime() got = %v, want %v", got, tt.expectedTime)
			}
			if got1 != 3 {
				t.Errorf("NextBeaconTime() got1 = %v, want %v", got1, 3)
			}
		})
	}
}
