package grpc

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestInsertSca(t *testing.T) {
	scs := make([]*scWithAddr, 0, 100)
	for i := 0; i < 100; i++ {
		sca := &scWithAddr{
			priority: rand.Intn(100),
		}
		// we insert in correct order, by priority
		scs = insert(scs, sca)
	}

	var previous int
	for _, sc := range scs {
		if sc.priority < previous {
			t.Fatalf("sc with order %d should have increased from %d", sc.priority, previous)
		}
		previous = sc.priority
	}
}

func TestInsertScaNegative(t *testing.T) {
	scs := make([]*scWithAddr, 0, 100)
	for i := -50; i < 50; i++ {
		sca := &scWithAddr{
			priority: i,
			order:    i,
		}
		// we insert in correct order, by priority
		scs = insert(scs, sca)
	}

	previous := -51
	for i, sc := range scs {
		if sc.priority < previous {
			t.Fatalf("sc with order %d should have increased from %d", sc.priority, previous)
		}
		if sc.priority != sc.order || sc.priority != i-50 {
			t.Fatalf("sc[%d] got changed: %v", i, sc)
		}
		previous = sc.priority
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

type fakeSubConn struct{ balancer.SubConn }

// newTestBalancer returns a balancer whose ready SubConns are addrs, in order of preference.
func newTestBalancer(addrs ...string) *fallbackBalancer {
	fb := &fallbackBalancer{scAddrs: make(map[balancer.SubConn]*scWithAddr)}
	for i, addr := range addrs {
		sc := &fakeSubConn{}
		fb.scAddrs[sc] = &scWithAddr{sc: sc, addr: addr, priority: i, order: i}
	}
	return fb
}

func TestPickerFailover(t *testing.T) {
	ctx := context.Background()
	stream := context.WithValue(ctx, streamPenaltyKey{}, time.Minute)
	stalled, stall := context.WithCancelCause(stream)
	stall(errStreamStalled)
	cancelled, cancel := context.WithCancel(stream)
	cancel()
	skip := context.WithValue(ctx, SkipCtxKey{}, true)
	unavailable := status.Error(codes.Unavailable, "node down")
	// drand nodes return plain errors, which reach the client as codes.Unknown
	unknownRound := status.Error(codes.Unknown, "can't retrieve beacon 7")

	type pick struct {
		reset bool // run a priority reset before the pick
		ctx   context.Context
		err   error // reported to Done
		want  string
	}
	tests := []struct {
		name  string
		picks []pick
	}{
		{"node error moves to the next node", []pick{{false, ctx, unavailable, "a"}, {false, ctx, nil, "b"}}},
		{"stream error moves to the next node", []pick{{false, stream, unknownRound, "a"}, {false, ctx, nil, "b"}}},
		{"stall moves to the next node", []pick{{false, stalled, status.Error(codes.Canceled, "stalled"), "a"}, {false, ctx, nil, "b"}}},
		{"caller cancel keeps the node", []pick{{false, cancelled, status.Error(codes.Canceled, "gone"), "a"}, {false, ctx, nil, "a"}}},
		{"unknown round keeps the node", []pick{{false, ctx, unknownRound, "a"}, {false, ctx, nil, "a"}}},
		{"skip moves to the next node", []pick{{false, skip, nil, "b"}, {false, ctx, nil, "b"}}},
		{"repeated errors reach the third node", []pick{{false, ctx, unavailable, "a"}, {false, ctx, unavailable, "b"}, {false, ctx, nil, "c"}}},
		{"stream errors reach the third node across resets", []pick{
			{false, stream, unknownRound, "a"}, {true, stream, unknownRound, "b"}, {true, stream, nil, "c"},
		}},
		{"expired penalty ends at the reset", []pick{{false, ctx, unavailable, "a"}, {true, ctx, nil, "a"}}},
		{"success keeps the node", []pick{{false, ctx, nil, "a"}, {false, ctx, nil, "a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb := newTestBalancer("a", "b", "c")
			p := &picker{fb: fb}
			for i, pk := range tt.picks {
				if pk.reset {
					fb.resetPriorities(time.Now())
				}
				res, err := p.Pick(balancer.PickInfo{Ctx: pk.ctx})
				if err != nil {
					t.Fatalf("Pick %d: %v", i, err)
				}
				if got := res.Metadata["target"][0]; got != pk.want {
					t.Fatalf("Pick %d = %s, want %s", i, got, pk.want)
				}
				res.Done(balancer.DoneInfo{Err: pk.err})
			}
		})
	}
}
