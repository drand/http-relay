package grpc

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInsertSca(t *testing.T) {
	scs := make([]*scWithAddr, 0, 100)
	for i := 0; i < 100; i++ {
		sca := &scWithAddr{
			order: rand.Intn(100),
		}
		// we insert in correct order, by priority
		scs = insert(scs, sca)
	}

	var previous int
	for _, sc := range scs {
		if sc.order < previous {
			t.Fatalf("sc with order %d should have increased from %d", sc.order, previous)
		}
		previous = sc.order
	}
}

func TestInsertNilsAreLast(t *testing.T) {
	scs := make([]*scWithAddr, 10, 100)
	for i := 10; i < 20; i++ {
		sca := &scWithAddr{
			order: rand.Intn(100),
		}
		// we insert in correct order, by priority
		scs = insert(scs, sca)
	}

	for _, sc := range scs[:10] {
		assert.NotNil(t, sc)
	}
	assert.Len(t, scs, 20)
}
