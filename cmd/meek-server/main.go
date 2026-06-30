// Command meek-server is a domain-frontable HTTP endpoint that
// terminates the meek-v1 protocol and forwards each session's bytes to
// a configured TCP upstream (typically a SOCKS5 inbound on localhost).
//
// Deployment: runs behind a CDN (Akamai DSA, CloudFront alt-domain
// distribution) that terminates TLS and forwards plain HTTP to the
// server's --listen address. The CDN's inner Host header carries the
// client's session over the fronted connection.
//
// Example, running on Linode behind nginx-as-TLS-terminator on :443
// with a sing-box SOCKS5 inbound on localhost:1080:
//
//	meek-server -listen :8080 -upstream 127.0.0.1:1080
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getlantern/lantern-box/protocol/meek"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "meek-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", ":8080", "address to listen on")
	upstream := flag.String("upstream", "", "upstream TCP address (e.g. 127.0.0.1:1080)")
	path := flag.String("path", "/", "URL path the server handles (other paths get 404)")
	maxBody := flag.Int("max-body", 256*1024, "max request/response body bytes per poll (matches the client's 256 KiB default)")
	holdoff := flag.Duration("holdoff", 50*time.Millisecond, "how long to wait for upstream bytes before responding")
	idleTimeout := flag.Duration("idle-timeout", 5*time.Minute, "session idle reap threshold")
	authToken := flag.String("auth-token", "", "shared secret required in the X-Meek-Auth header; empty disables auth (open relay — only for local testing)")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	if *upstream == "" {
		return errors.New("-upstream is required")
	}

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	if *authToken == "" {
		logger.Warn("meek-server: -auth-token not set; running as an OPEN RELAY into the upstream (fine for local testing, never in production)")
	}

	srv, err := meek.NewServer(meek.ServerConfig{
		Upstream:           *upstream,
		MaxBodyBytes:       *maxBody,
		ResponseHoldoff:    *holdoff,
		SessionIdleTimeout: *idleTimeout,
		AuthToken:          *authToken,
		Logger:             logger,
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle(*path, srv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok sessions=%d\n", srv.SessionCount())
	})

	httpServer := &http.Server{
		Addr:    *listen,
		Handler: mux,
		// Bound slow/abusive clients. Generous vs meek's poll model: each POST
		// sends its body then gets a response within ResponseHoldoff, so these
		// only kill genuinely-stuck connections, not normal polls.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("meek-server starting",
			slog.String("listen", *listen),
			slog.String("upstream", *upstream),
			slog.String("path", *path),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	case sig := <-sigCh:
		logger.Info("meek-server shutting down", slog.String("signal", sig.String()))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}
	return nil
}
