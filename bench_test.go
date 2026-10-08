package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	proto "github.com/drand/drand/v2/protobuf/drand"
	"github.com/drand/http-relay/grpc"
	"github.com/drand/http-relay/internal/fakenode"
)

// Run with -benchtime=Nx: every "next" iteration waits for a real round, so
// time-based benchtime stops after a single round.
//
//	go test -run '^$' -bench . -benchtime 10x -benchmem

// benchRelay is a fake node, a relay gRPC client and an HTTP server wired together.
type benchRelay struct {
	node   *fakenode.Node
	client *grpc.Client
	http   *http.Client
	url    string
}

// quietLogs silences the balancer and resolver, which log through slog.Default.
var quietLogs sync.Once

func newBenchRelay(b *testing.B, opts fakenode.Options) *benchRelay {
	b.Helper()
	quietLogs.Do(func() { slog.SetDefault(slog.New(slog.DiscardHandler)) })

	node, err := fakenode.Start(opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(node.Stop)

	client, err := grpc.NewClient("fallback:///"+node.Addr(), slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { client.Close() }) //nolint:errcheck // benchmark teardown

	server := httptest.NewServer(drandHandler(client, io.Discard))
	b.Cleanup(server.Close)

	return &benchRelay{
		node:   node,
		client: client,
		http:   &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4096}},
		url:    server.URL,
	}
}

// get fetches a beacon over HTTP and returns it with the time the body was fully read.
func (r *benchRelay) get(b *testing.B, path string) (*grpc.HexBeacon, time.Time) {
	b.Helper()

	resp, err := r.http.Get(r.url + path)
	if err != nil {
		b.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // nothing to do with a close error on a read body
	received := time.Now()
	if err != nil {
		b.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
	}
	beacon := new(grpc.HexBeacon)
	if err := json.Unmarshal(body, beacon); err != nil {
		b.Fatalf("GET %s: invalid beacon %q: %v", path, body, err)
	}
	return beacon, received
}

func (r *benchRelay) chainPath() string {
	return "/v2/chains/" + hex.EncodeToString(r.node.ChainHash())
}

// reportLatency reports percentiles of the emit-to-delivery latency in milliseconds.
func reportLatency(b *testing.B, latencies []time.Duration) {
	b.Helper()
	if len(latencies) == 0 {
		return
	}
	slices.Sort(latencies)
	percentile := func(p float64) float64 {
		i := min(int(float64(len(latencies))*p), len(latencies)-1)
		return float64(latencies[i]) / float64(time.Millisecond)
	}
	b.ReportMetric(percentile(0.5), "p50-ms")
	b.ReportMetric(percentile(0.9), "p90-ms")
	b.ReportMetric(percentile(0.99), "p99-ms")
	b.ReportMetric(percentile(1), "max-ms")
}

// waitRounds runs b.N rounds of `clients` concurrent fetches of a path that
// blocks until the next beacon, and reports emit-to-delivery latency.
func (r *benchRelay) waitRounds(b *testing.B, clients int, path func() string) {
	b.Helper()

	latencies := make([]time.Duration, 0, b.N*clients)
	var mu sync.Mutex
	b.ResetTimer()
	for range b.N {
		var wg sync.WaitGroup
		for range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				beacon, received := r.get(b, path())
				latency := received.Sub(r.node.EmitTime(beacon.Round))
				mu.Lock()
				latencies = append(latencies, latency)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	b.StopTimer()
	reportLatency(b, latencies)
}

func BenchmarkGetBeacon(b *testing.B) {
	r := newBenchRelay(b, fakenode.Options{})
	paths := []struct{ name, path string }{
		{"v1", "/public/1"},
		{"v1-chainhash", "/" + hex.EncodeToString(r.node.ChainHash()) + "/public/1"},
		{"v2-beaconid", "/v2/beacons/default/rounds/1"},
		{"v2-chainhash", r.chainPath() + "/rounds/1"},
	}
	for _, p := range paths {
		b.Run(p.name, func(b *testing.B) {
			for range b.N {
				r.get(b, p.path)
			}
		})
		b.Run(p.name+"/parallel", func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r.get(b, p.path)
				}
			})
		})
	}
}

func BenchmarkGetLatest(b *testing.B) {
	for _, rtt := range []time.Duration{0, 5 * time.Millisecond} {
		r := newBenchRelay(b, fakenode.Options{RTT: rtt})
		paths := []struct{ name, path string }{
			{"v1", "/public/latest"},
			{"v2-beaconid", "/v2/beacons/default/rounds/latest"},
			{"v2-chainhash", r.chainPath() + "/rounds/latest"},
		}
		for _, p := range paths {
			name := fmt.Sprintf("rtt=%s/%s", rtt, p.name)
			b.Run(name, func(b *testing.B) {
				for range b.N {
					r.get(b, p.path)
				}
			})
			b.Run(name+"/parallel", func(b *testing.B) {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						r.get(b, p.path)
					}
				})
			})
		}
	}
}

// BenchmarkGetNext measures how long after the node emits a round the HTTP
// client holding /rounds/next receives it, for growing numbers of waiters.
func BenchmarkGetNext(b *testing.B) {
	for _, clients := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			r := newBenchRelay(b, fakenode.Options{})
			r.waitRounds(b, clients, func() string { return "/v2/beacons/default/rounds/next" })
		})
	}
}

// BenchmarkGetBeaconNextRound requests the upcoming round number explicitly,
// which takes the Next-then-GetBeacon path in getBeacon.
func BenchmarkGetBeaconNextRound(b *testing.B) {
	for _, clients := range []int{1, 100} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			r := newBenchRelay(b, fakenode.Options{})
			r.waitRounds(b, clients, func() string {
				return fmt.Sprintf("/v2/beacons/default/rounds/%d", r.node.Round()+1)
			})
		})
	}
}

// BenchmarkClientNext isolates the gRPC watch path from HTTP: node emit to
// grpc.Client.Next returning.
func BenchmarkClientNext(b *testing.B) {
	for _, clients := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			r := newBenchRelay(b, fakenode.Options{})
			md := &proto.Metadata{BeaconID: fakenode.BeaconID}
			latencies := make([]time.Duration, 0, b.N*clients)
			var mu sync.Mutex
			b.ResetTimer()
			for range b.N {
				var wg sync.WaitGroup
				for range clients {
					wg.Add(1)
					go func() {
						defer wg.Done()
						beacon, err := r.client.Next(context.Background(), md)
						received := time.Now()
						if err != nil {
							b.Error(err)
							return
						}
						latency := received.Sub(r.node.EmitTime(beacon.Round))
						mu.Lock()
						latencies = append(latencies, latency)
						mu.Unlock()
					}()
				}
				wg.Wait()
			}
			b.StopTimer()
			reportLatency(b, latencies)
		})
	}
}

// BenchmarkClientGetBeacon isolates a unary PublicRand round trip through the relay client.
func BenchmarkClientGetBeacon(b *testing.B) {
	r := newBenchRelay(b, fakenode.Options{})
	md := &proto.Metadata{BeaconID: fakenode.BeaconID}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := r.client.GetBeacon(context.Background(), md, 1); err != nil {
				b.Error(err)
			}
		}
	})
}
