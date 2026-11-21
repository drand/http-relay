package grpc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
)

var (
	errClientClosed  = errors.New("grpc client closed")
	errStreamStalled = errors.New("no beacon received in time")
)

const (
	defaultMinBackoff = 100 * time.Millisecond
	defaultMaxBackoff = 5 * time.Second
	// stallPeriods is how many periods a stream may stay silent before it is reopened.
	stallPeriods = 2
)

type nextResult struct {
	beacon *HexBeacon
	err    error
}

// sharedStream fans one PublicRandStream out to every Next caller of a chain.
// The node keys its streams by peer address, so a second stream from the same
// relay evicts the first: there must never be more than one per chain.
// Client.nextMu guards all fields.
type sharedStream struct {
	waiters  map[chan nextResult]struct{}
	running  bool
	failures uint      // consecutive stream failures, reset when a beacon arrives
	retryAt  time.Time // earliest time to reopen the stream
	// failed holds the nodes that failed the stream since the last beacon, oldest failure first. The stream avoids
	// them instead of penalizing them for every chain: a node may fail one chain only, or the chain may have halted.
	failed []string
}

// Next blocks until the network designated in the metadata emits its next beacon. Concurrent callers for the same
// chain share one upstream stream, which stays open until it failed on every node, stalls with no caller waiting or
// the Client is closed. A late beacon does not fail the callers: they wait until it arrives or their context ends.
func (c *Client) Next(ctx context.Context, m *proto.Metadata) (*HexBeacon, error) {
	// A beacon ID and a chain hash can designate the same chain, the canonical hash keeps them on one stream.
	// It also means unknown chains fail here, before they can create any state.
	info, err := c.GetChainInfo(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("resolving chain: %w", err)
	}
	if len(info.Hash) == 0 {
		// chains without a hash would all share the "" key, and each other's beacons
		return nil, errors.New("chain info has no chain hash")
	}
	key := info.Hash.String()
	// buffered so that the stream goroutine never blocks on a caller that left
	ch := make(chan nextResult, 1)

	c.nextMu.Lock()
	if c.ctx.Err() != nil {
		c.nextMu.Unlock()
		return nil, errClientClosed
	}
	s, ok := c.streams[key]
	if !ok {
		s = &sharedStream{waiters: make(map[chan nextResult]struct{})}
		c.streams[key] = s
	}
	s.waiters[ch] = struct{}{}
	if !s.running {
		s.running = true
		stall := stallPeriods * time.Duration(info.Period) * time.Second
		go c.runStream(s, &proto.Metadata{ChainHash: info.Hash}, stall)
	}
	c.nextMu.Unlock()

	select {
	case <-ctx.Done():
		c.nextMu.Lock()
		delete(s.waiters, ch)
		c.nextMu.Unlock()
		return nil, ctx.Err()
	case res := <-ch:
		return res.beacon, res.err
	}
}

// runStream serves the waiters of s until the stream failed on every node or the Client is closed, then fails the
// remaining waiters. A stalled stream is reopened as long as it has waiters, which keep waiting. Each reopen goes to a
// node that has not failed since the last beacon, or else to the one that failed longest ago, so a halted chain walks
// the nodes round after round. The next call to Next starts it again, still avoiding those nodes.
//
// A reopen waits for the full backoff only when it cannot go to a node not yet tried since the last beacon, so the
// callers of a chain failing on every node do not wait one backoff per node. The stream fails its callers at once when
// no node could be picked.
func (c *Client) runStream(s *sharedStream, m *proto.Metadata, stall time.Duration) {
	// consecutive errors other than stalls, and nodes tried since the last beacon
	errs := 0
	tried := make(map[string]bool)
	for {
		addr, err := c.receive(s, m, stall)
		if c.ctx.Err() != nil {
			err = errClientClosed
		} else {
			c.log.Error("public rand stream error", "err", err)
		}

		c.nextMu.Lock()
		s.failures++
		s.retryAt = time.Now().Add(c.backoff(s.failures))
		if s.failures == 1 {
			// a beacon arrived since the previous failure
			errs = 0
			clear(tried)
		}
		stalled := errors.Is(err, errStreamStalled)
		if stalled {
			// a halt can last for hours: unrelated errors that far apart must not add up and fail the callers
			errs = 0
		} else {
			errs++
		}
		if addr != "" {
			s.failed = append(slices.DeleteFunc(s.failed, func(a string) bool { return a == addr }), addr)
		}
		if len(s.waiters) > 0 && c.ctx.Err() == nil && errs < c.nodes && (addr != "" || stalled) {
			if addr != "" && !tried[addr] {
				// The picker reopens on another node, at most once per node until a beacon arrives. With a single
				// READY node it reopens on the same one: the node must first see the previous stream end.
				tried[addr] = true
				s.retryAt = time.Now().Add(c.minBackoff)
			}
			c.nextMu.Unlock()
			continue
		}
		s.running = false
		for ch := range s.waiters {
			ch <- nextResult{err: err}
			delete(s.waiters, ch)
		}
		c.nextMu.Unlock()
		return
	}
}

// receive serves the waiters of s from one stream until it fails. It returns the node the stream used, "" if none.
func (c *Client) receive(s *sharedStream, m *proto.Metadata, stall time.Duration) (string, error) {
	c.nextMu.Lock()
	wait := time.Until(s.retryAt)
	avoid := slices.Clone(s.failed)
	c.nextMu.Unlock()
	if wait > 0 {
		select {
		case <-c.ctx.Done():
			return "", c.ctx.Err()
		case <-time.After(wait):
		}
	}

	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	ctx, served := TrackNode(AvoidNodes(ctx, avoid...))
	// a node can keep the connection up without emitting beacons
	var watchdog *time.Timer
	if stall > 0 {
		watchdog = time.AfterFunc(stall, func() { cancel(errStreamStalled) })
		defer watchdog.Stop()
	}
	stream, err := c.pc.PublicRandStream(ctx, &proto.PublicRandRequest{Metadata: m})
	if err != nil {
		err = streamError(ctx, err)
		if errors.Is(err, errStreamStalled) {
			// the call waited in the picker, no node is to blame
			return "", fmt.Errorf("opening public rand stream: %w", err)
		}
		return served(), fmt.Errorf("opening public rand stream: %w", err)
	}
	// the stall window measures the node, not the wait for a READY one
	if watchdog != nil && !watchdog.Reset(stall) {
		return "", fmt.Errorf("opening public rand stream: %w", errStreamStalled)
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return served(), fmt.Errorf("public rand stream: %w", streamError(ctx, err))
		}
		if watchdog != nil {
			watchdog.Reset(stall)
		}

		c.nextMu.Lock()
		s.failures = 0
		s.failed = nil
		for ch := range s.waiters {
			// handlers mutate the beacon they get, so each waiter needs its own
			ch <- nextResult{beacon: NewHexBeacon(resp)}
			delete(s.waiters, ch)
		}
		c.nextMu.Unlock()
	}
}

// streamError returns errStreamStalled when the watchdog is what ended the stream, err otherwise.
func streamError(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStreamStalled) {
		return errStreamStalled
	}
	return err
}

// backoff returns how long to wait before reopening a stream that failed the given number of times in a row.
func (c *Client) backoff(failures uint) time.Duration {
	wait := c.minBackoff << min(failures-1, 16)
	return min(wait, c.maxBackoff)
}
