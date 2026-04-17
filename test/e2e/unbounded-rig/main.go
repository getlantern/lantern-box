// Command unbounded-rig runs freddie (signaling), the broflake egress
// (SOCKS5 over QUIC-over-WebSocket), and a native broflake widget in a single
// process. It exists to provide the server-side topology for a live-machine
// e2e test of lantern-box's unbounded outbound.
//
// The binary is deployed to an ephemeral VM (currently a DigitalOcean
// droplet, see .github/workflows/e2e.yaml). A CI-side lantern-box then points
// its unbounded outbound at the VM's public IP. This validates the full
// chain — real TLS, real STUN, real NAT traversal, a real quic-go QUIC
// transport between two distinct processes — which the in-process test at
// test/e2e/unbounded_test.go cannot.
//
// Wire-compatible knobs:
//
//	FREDDIE_ADDR       listen address for the signaling server (default :9000)
//	EGRESS_ADDR        listen address for the egress server    (default :8000)
//	STUN_SERVER        STUN server URL for widget's ICE batch
//	                   (default stun:stun.l.google.com:19302)
//	TLS_CERT_FILE      PEM path; used by both freddie and egress TLS
//	TLS_KEY_FILE       PEM path; used by both freddie and egress TLS
//
// The egress wraps a go-socks5 server so SOCKS5 CONNECT requests exit to the
// public internet. The widget is configured with ClientType=widget and
// connects to freddie as a volunteer; it pairs with whichever consumer
// arrives first.
package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/armon/go-socks5"
	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/getlantern/broflake/egress"
	"github.com/getlantern/broflake/freddie"
)

func main() {
	freddieAddr := envDefault("FREDDIE_ADDR", ":9000")
	egressAddr := envDefault("EGRESS_ADDR", ":8000")
	stunServer := envDefault("STUN_SERVER", "stun:stun.l.google.com:19302")
	tlsCertFile := os.Getenv("TLS_CERT_FILE")
	tlsKeyFile := os.Getenv("TLS_KEY_FILE")

	if tlsCertFile == "" || tlsKeyFile == "" {
		log.Fatal("TLS_CERT_FILE and TLS_KEY_FILE are required")
	}

	cert, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
	if err != nil {
		log.Fatalf("load cert/key: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Println("shutting down")
		cancel()
	}()

	UBCommon.SetDebugLogger(log.New(os.Stderr, "[broflake] ", log.LstdFlags))

	// freddie — TLS so the consumer-side kindling transport can hit it.
	startFreddie(ctx, freddieAddr, tlsCertFile, tlsKeyFile)

	// egress — SOCKS5 server wrapped in the broflake egress layer. The tls.Config
	// passed to egress.NewListener is used for the INNER QUIC handshake (QUIC
	// requires TLS 1.3) — not the outer WebSocket. The outer WebSocket is plain
	// ws:// over TCP. The consumer validates the inner cert via
	// InsecureDoNotVerifyClientCert.
	egressURL := startEgress(ctx, egressAddr, cert)

	// Readiness gate — widget won't pair until freddie's /v1/ accepts polls.
	// The CI runner also waits on freddie externally, but widget is in-proc
	// with freddie so we wait locally before connecting.
	waitForFreddie(ctx, "https://127.0.0.1"+firstPort(freddieAddr), 20*time.Second)

	freddieURL := "https://127.0.0.1" + firstPort(freddieAddr)
	startWidget(ctx, freddieURL, egressURL, stunServer)

	log.Printf("rig ready: freddie=%s egress=%s", freddieURL, egressURL)
	<-ctx.Done()
}

func startFreddie(ctx context.Context, addr, cert, key string) {
	f, err := freddie.New(ctx, addr)
	if err != nil {
		log.Fatalf("freddie.New: %v", err)
	}
	go func() {
		if err := f.ListenAndServeTLS(cert, key); err != nil && err != http.ErrServerClosed {
			log.Printf("freddie exited: %v", err)
		}
	}()
	log.Printf("freddie listening on %s", addr)
}

func startEgress(ctx context.Context, addr string, cert tls.Certificate) string {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("egress listen: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"broflake"},
		InsecureSkipVerify: true,
	}
	ll, err := egress.NewListener(ctx, l, tlsConfig)
	if err != nil {
		log.Fatalf("egress.NewListener: %v", err)
	}
	conf := &socks5.Config{}
	proxy, err := socks5.New(conf)
	if err != nil {
		log.Fatalf("socks5.New: %v", err)
	}
	go func() {
		if err := proxy.Serve(ll); err != nil {
			log.Printf("egress SOCKS5 exited: %v", err)
		}
	}()
	log.Printf("egress listening on %s", addr)
	return "ws://127.0.0.1" + firstPort(addr)
}

func startWidget(ctx context.Context, freddieURL, egressURL, stunServer string) {
	bfOpt := UBClientcore.NewDefaultBroflakeOptions()
	bfOpt.ClientType = "widget"
	// Match the in-process test; small pools are fine for a single consumer.
	bfOpt.CTableSize = 2
	bfOpt.PTableSize = 2

	rtcOpt := UBClientcore.NewDefaultWebRTCOptions()
	rtcOpt.DiscoverySrv = freddieURL
	rtcOpt.STUNBatch = func(_ uint32) ([]string, error) {
		return []string{stunServer}, nil
	}
	// The widget polls freddie over TLS. Our freddie is running on the same
	// host with a self-signed cert, so the widget's http client has to skip
	// verification. In production, widgets hit a real freddie deployment with
	// a real cert and this is not needed.
	rtcOpt.HTTPClient = &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	egOpt := UBClientcore.NewDefaultEgressOptions()
	egOpt.Addr = egressURL

	_, ui, err := UBClientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
	if err != nil {
		log.Fatalf("NewBroflake: %v", err)
	}

	// Stop on shutdown so the widget releases its freddie registration.
	var once sync.Once
	stop := func() { once.Do(ui.Stop) }
	go func() {
		<-ctx.Done()
		stop()
	}()
}

func waitForFreddie(ctx context.Context, target string, timeout time.Duration) {
	client := &http.Client{
		Timeout: 1 * time.Second,
		// freddie's cert is self-signed; the rig trusts its own cert.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		resp, err := client.Get(target + "/v1/")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("freddie never became ready at %s", target)
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// firstPort extracts ":port" from a listen spec like ":9000" or "0.0.0.0:9000".
// Used to build a loopback URL the rig itself can use for waitForFreddie.
func firstPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return ":" + port
}

