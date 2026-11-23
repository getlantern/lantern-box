//go:build with_quic

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/sniff"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// TestHedgeMultiDomainPage loads a page whose resources come from several
// sites through a hedge, where First runs through a fake censor that
// blackholes TLS by SNI and Then is an uncensored proxy. Every resource must
// load. A blocked site must reach the proxy after one commit timeout, a
// second host of that site must skip the censored path entirely, and open
// sites must never use the proxy.
func TestHedgeMultiDomainPage(t *testing.T) {
	// Generous enough that a loaded CI runner doesn't push an open site's
	// dial or first reply past it; at 500ms one did.
	const commitTimeout = 2 * time.Second
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Host)
	}))
	defer origin.Close()
	originAddr := origin.Listener.Addr().String()

	censor := startTestSOCKS(t, originAddr, map[string]bool{"blocked.com": true, "cdn.blocked.com": true})
	proxy := startTestSOCKS(t, originAddr, nil)

	boxCtx := box.BaseContext()
	var clientPort uint16
	startBoxOnFreePorts(t, boxCtx, func(port func(string) uint16) option.Options {
		clientPort = port("tcp")
		return mustOptions(boxCtx, t, fmt.Sprintf(`{
			"log": {"level": "error"},
			"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
			"outbounds": [
				{"type": "hedge", "tag": "smart", "first": "censored", "then": "proxy", "commit_timeout": "%s"},
				{"type": "socks", "tag": "censored", "server": "127.0.0.1", "server_port": %d},
				{"type": "socks", "tag": "proxy", "server": "127.0.0.1", "server_port": %d}
			],
			"route": {"final": "smart"}
		}`, clientPort, commitTimeout, censor.port, proxy.port))
	})

	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", clientPort))
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}}
	fetch := func(host string) time.Duration {
		t.Helper()
		start := time.Now()
		resp, err := client.Get("https://" + host + "/")
		require.NoError(t, err, host)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, host, string(body))
		return time.Since(start)
	}

	fetch("www.open.com")
	fetch("img.open.com")
	first := fetch("blocked.com")
	second := fetch("cdn.blocked.com")

	require.GreaterOrEqual(t, first, commitTimeout, "the first blocked fetch waits out the commit timeout")
	require.Less(t, first, commitTimeout+2*time.Second)
	require.Less(t, second, commitTimeout, "another host of a known-blocked site skips the censored path")

	require.Equal(t, 1, censor.count("blocked.com"))
	require.Zero(t, censor.count("cdn.blocked.com"), "the censored path must not be tried again for the site")
	require.Equal(t, 1, proxy.count("blocked.com"))
	require.Equal(t, 1, proxy.count("cdn.blocked.com"))
	require.Zero(t, proxy.count("www.open.com"), "open sites must not use the proxy")
	require.Zero(t, proxy.count("img.open.com"), "open sites must not use the proxy")
}

// TestHedgeSwitchableFromBox builds a hedge from JSON, as radiance does, and
// reaches its switch through the box's outbound manager, as radiance will to
// apply a feature flag. The client-info injector radiance installs wraps
// outbounds, so it is installed here too: it must leave the hedge unwrapped.
func TestHedgeSwitchableFromBox(t *testing.T) {
	boxCtx := box.BaseContext()
	injector := clientcontext.NewInjector(func() clientcontext.ClientInfo { return clientcontext.ClientInfo{} }, "smart", "a", "b")
	require.NoError(t, injector.Install(boxCtx))
	client, err := sbox.New(sbox.Options{Context: boxCtx, Options: mustOptions(boxCtx, t, `{
		"log": {"level": "error"},
		"outbounds": [
			{"type": "hedge", "tag": "smart", "first": "a", "then": "b", "disabled": true},
			{"type": "direct", "tag": "a"},
			{"type": "direct", "tag": "b"}
		]
	}`)})
	require.NoError(t, err)
	require.NoError(t, client.Start())
	t.Cleanup(func() { client.Close() })
	out, ok := client.Outbound().Outbound("smart")
	require.True(t, ok)
	sw, ok := out.(lAdapter.Switchable)
	require.True(t, ok, "the hedge must be reachable as adapter.Switchable")
	require.False(t, sw.Enabled())
	sw.SetEnabled(true)
	require.True(t, sw.Enabled())
}

// TestHedgeSurvivesRemovingFirst removes First from a running box, as a
// client does when the server stops sending it. The removal must succeed, and
// the hedge must go on serving through Then.
func TestHedgeSurvivesRemovingFirst(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()
	proxy := startTestSOCKS(t, origin.Listener.Addr().String(), nil)

	boxCtx := box.BaseContext()
	var clientPort uint16
	client := startBoxOnFreePorts(t, boxCtx, func(port func(string) uint16) option.Options {
		clientPort = port("tcp")
		return mustOptions(boxCtx, t, fmt.Sprintf(`{
			"log": {"level": "error"},
			"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
			"outbounds": [
				{"type": "hedge", "tag": "smart", "first": "proxyless", "then": "proxy"},
				{"type": "direct", "tag": "proxyless"},
				{"type": "socks", "tag": "proxy", "server": "127.0.0.1", "server_port": %d}
			],
			"route": {"final": "smart"}
		}`, clientPort, proxy.port))
	})

	require.NoError(t, client.Outbound().Remove("proxyless"), "First is not a dependency the manager may refuse to remove")

	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", clientPort))
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}}
	resp, err := hc.Get("https://site.example/")
	require.NoError(t, err, "with First gone, the hedge serves through Then")
	resp.Body.Close()
	require.Equal(t, 1, proxy.count("site.example"))
}

// testSOCKS is a SOCKS5 CONNECT server that sends every connection to one
// origin. With a blocklist it acts as a censor: it reads the ClientHello and
// silently drops connections whose SNI is listed.
type testSOCKS struct {
	port  uint16
	mu    sync.Mutex
	hosts map[string]int
}

func (s *testSOCKS) count(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hosts[host]
}

func startTestSOCKS(t *testing.T, origin string, blocked map[string]bool) *testSOCKS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	s := &testSOCKS{port: uint16(ln.Addr().(*net.TCPAddr).Port), hosts: map[string]int{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, origin, blocked)
		}
	}()
	return s
}

func (s *testSOCKS) serve(c net.Conn, origin string, blocked map[string]bool) {
	defer c.Close()
	host, err := socksHandshake(c)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.hosts[host]++
	s.mu.Unlock()

	hello, err := readTLSRecord(c)
	if err != nil {
		return
	}
	if blocked != nil {
		md := &adapter.InboundContext{}
		if sniff.TLSClientHello(context.Background(), md, bytes.NewReader(hello)) == nil && blocked[md.Domain] {
			_, _ = io.Copy(io.Discard, c)
			return
		}
	}
	upstream, err := net.Dial("tcp", origin)
	if err != nil {
		return
	}
	defer upstream.Close()
	if _, err := upstream.Write(hello); err != nil {
		return
	}
	go func() { _, _ = io.Copy(upstream, c); upstream.Close() }()
	_, _ = io.Copy(c, upstream)
}

// socksHandshake accepts a no-auth SOCKS5 CONNECT and returns the host.
func socksHandshake(c net.Conn) (string, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(c, make([]byte, head[1])); err != nil {
		return "", err
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return "", err
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", err
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	default:
		return "", fmt.Errorf("address type %d", req[3])
	}
	if _, err := io.ReadFull(c, make([]byte, 2)); err != nil { // port
		return "", err
	}
	_, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	return host, err
}

// readTLSRecord reads one whole TLS record, which holds the ClientHello.
func readTLSRecord(c net.Conn) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(c, header); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint16(header[3:5]))
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	return append(header, body...), nil
}
