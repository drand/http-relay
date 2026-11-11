package grpc

import (
	"context"
	"math/rand"
	"testing"

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
	stalled, stall := context.WithCancelCause(ctx)
	stall(errStreamStalled)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	skip := context.WithValue(ctx, SkipCtxKey{}, true)
	unavailable := status.Error(codes.Unavailable, "node down")

	type pick struct {
		ctx  context.Context
		err  error // reported to Done
		want string
	}
	tests := []struct {
		name  string
		picks []pick
	}{
		{"node error moves to the next node", []pick{{ctx, unavailable, "a"}, {ctx, nil, "b"}}},
		{"stall moves to the next node", []pick{{stalled, status.Error(codes.Canceled, "stalled"), "a"}, {ctx, nil, "b"}}},
		{"caller cancel keeps the node", []pick{{cancelled, status.Error(codes.Canceled, "gone"), "a"}, {ctx, nil, "a"}}},
		{"unknown round keeps the node", []pick{{ctx, status.Error(codes.NotFound, "no round"), "a"}, {ctx, nil, "a"}}},
		{"skip moves to the next node", []pick{{skip, nil, "b"}, {ctx, nil, "b"}}},
		{"repeated errors reach the third node", []pick{{ctx, unavailable, "a"}, {ctx, unavailable, "b"}, {ctx, nil, "c"}}},
		{"success keeps the node", []pick{{ctx, nil, "a"}, {ctx, nil, "a"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &picker{fb: newTestBalancer("a", "b", "c")}
			for i, pk := range tt.picks {
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
