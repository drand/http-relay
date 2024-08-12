package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/drand/http-server/grpc"
)

var (
	version     = "drand-http-server-v2.0.1"
	metricFlag  = flag.String("metrics", "localhost:9999", "The flag to set the interface for metrics. Defaults to localhost:9999")
	httpBind    = flag.String("bind", "localhost:8080", "The address to bind the http server to")
	grpcURL     = flag.String("grpc-connect", "localhost:4444", "The URL and port to your drand node's grpc port, e.g. pl1-rpc.testnet.drand.sh:443 you can add fallback nodes by separating them with a comma: pl1-rpc.testnet.drand.sh:443,pl2-rpc.testnet.drand.sh:443")
	goVersion   = flag.Bool("version", false, "Displays the current server version.")
	requireAuth = flag.Bool("enable-auth", false, "Forces JWT authentication on V2 API using the JWT secret from the AUTH_TOKEN env variable.")
	verbose     = flag.Bool("verbose", false, "Prints as many logs as possible.")
	jsonFlag    = flag.Bool("json", false, "Prints logs in JSON format.")
	frontrun    = flag.Int64("frontrun", 0, "When waiting for the next round, start the query this amount of ms earlier to counteract network latency.")
	_           = flag.Bool("insecure", false, "deprecated flag")
	_           = flag.String("hash-list", "", "deprecated flag")
)

func init() {
	flag.Parse()
	slog.SetLogLoggerLevel(getLogLevel())
	if *frontrun > 0 {
		FrontrunTiming = time.Duration(*frontrun) * time.Millisecond
	}
}

func main() {
	if *goVersion {
		log.Fatal("drand http server version: ", version)
	}

	nodesAddr := strings.Split(*grpcURL, ",")
	for _, nodeAdd := range nodesAddr {
		_, _, err := net.SplitHostPort(nodeAdd)
		if err != nil {
			log.Fatalf("Unable to parse --grpc flag correctly, please provide valid node URLs. On %q, got err: %v", nodeAdd, err)
		}
	}

	client, err := grpc.NewClient("fallback:///"+*grpcURL, slog.Default())
	if err != nil {
		log.Fatal("Failed to create client", "address", nodesAddr, "error", err)
	}
	defer client.Close()

	go serveMetrics()

	slog.Info("Starting http relay", "version", version, "client", client)

	// The HTTP Server
	server := &http.Server{Addr: *httpBind, Handler: drandHandler(client)}

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to exit gracefully
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		defer serverStopCtx()
		s := <-sig

		slog.Info("Caught interrupt, shutting down...", "signal", s.String())

		// Shutdown signal with grace period of 30 seconds
		shutdownCtx, cancel := context.WithTimeout(serverCtx, 30*time.Second)
		defer cancel()
		go func() {
			<-shutdownCtx.Done()
			if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
				slog.Error("graceful shutdown timed out.. forcing exit")
				return
			}
		}()

		// Trigger graceful shutdown
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("server Shutdown error", "err", err)
			return
		}
	}()

	// Run the server
	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server error", "err", err)
		return
	}

	// Wait for server context to be stopped
	<-serverCtx.Done()
	slog.Info("drand http server stopped")
}

func getLogLevel() slog.Level {
	if *verbose {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
