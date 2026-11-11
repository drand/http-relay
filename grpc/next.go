package grpc

import (
	"context"
	"errors"
	"fmt"
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
// remaining waiters. A stalled stream is reopened as long as it has waiters, which keep waiting. The next call to
// Next starts it again.
func (c *Client) runStream(s *sharedStream, m *proto.Metadata, stall time.Duration) {
	errs := 0 // errors other than stalls since the last beacon
	for {
		err := c.receive(s, m, stall)
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
		}
		if !errors.Is(err, errStreamStalled) {
			errs++
		}
		// the picker reopens the stream on another node
		if len(s.waiters) > 0 && c.ctx.Err() == nil && errs < c.nodes {
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

func (c *Client) receive(s *sharedStream, m *proto.Metadata, stall time.Duration) error {
	c.nextMu.Lock()
	wait := time.Until(s.retryAt)
	c.nextMu.Unlock()
	if wait > 0 {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(wait):
		}
	}

	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	ctx = context.WithValue(ctx, streamPenaltyKey{}, c.streamPenalty(stall))
	// a node can keep the connection up without emitting beacons
	var watchdog *time.Timer
	if stall > 0 {
		watchdog = time.AfterFunc(stall, func() { cancel(errStreamStalled) })
		defer watchdog.Stop()
	}
	stream, err := c.pc.PublicRandStream(ctx, &proto.PublicRandRequest{Metadata: m})
	if err != nil {
		return fmt.Errorf("opening public rand stream: %w", streamError(ctx, err))
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("public rand stream: %w", streamError(ctx, err))
		}
		if watchdog != nil {
			watchdog.Reset(stall)
		}

		c.nextMu.Lock()
		s.failures = 0
		for ch := range s.waiters {
			// handlers mutate the beacon they get, so each waiter needs its own
			ch <- nextResult{beacon: NewHexBeacon(resp)}
			delete(s.waiters, ch)
		}
		c.nextMu.Unlock()
	}
}

// streamPenalty returns how long the picker keeps a node that failed the stream behind the others: until every other
// node had time to stall, so consecutive reopens go through every node.
func (c *Client) streamPenalty(stall time.Duration) time.Duration {
	return time.Duration(max(c.nodes-1, 1))*(stall+c.maxBackoff) + c.maxBackoff
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
