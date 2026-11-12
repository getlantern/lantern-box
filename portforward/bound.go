package portforward

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
	"github.com/sagernet/sing/common/control"
)

// BoundTo has discovery, and every request the Forwarder then sends to the
// gateway, go through sockets fn controls, such as one bound to the physical
// interface while a VPN tunnel holds the default route. Unbound, the UPnP
// search goes out the tunnel and fails, and PCP and NAT-PMP requests are
// routed into it and never answered. It also sets the Forwarder's local IP
// control (see SetLocalIPControl). A nil fn changes nothing.
func BoundTo(fn control.Func) Option {
	return func(o *options) { o.control = fn }
}

// ssdpAddr is where an SSDP search is sent. A var so tests can answer it.
var ssdpAddr = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

// ssdpWait bounds how long one search waits for a gateway to answer, within
// the caller's ctx: the request's MX, as goupnp waits.
var ssdpWait = 2 * time.Second

// deviceFetchTimeout bounds fetching one gateway's device description, as
// goupnp does.
const deviceFetchTimeout = 3 * time.Second

// discoverIGDv2Bound is discoverIGDv2 through sockets fn controls.
func discoverIGDv2Bound(ctx context.Context, fn control.Func) (igdClient, error) {
	return searchIGD(ctx, fn, []string{internetgateway2.URN_WANIPConnection_2},
		func(_ string, root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient {
			clients, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, loc)
			if err != nil || len(clients) == 0 {
				return nil
			}
			clients[0].SOAPClient.HTTPClient = *hc
			return wanIPv2Wrapper{c: clients[0]}
		})
}

// discoverIGDv1Bound is discoverIGDv1 through sockets fn controls. It looks
// for both of IGDv1's connection services in one search, where goupnp makes
// two, so the bound path fits the time callers give discovery.
func discoverIGDv1Bound(ctx context.Context, fn control.Func) (igdClient, error) {
	return searchIGD(ctx, fn, []string{internetgateway1.URN_WANIPConnection_1, internetgateway1.URN_WANPPPConnection_1},
		func(target string, root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient {
			if target == internetgateway1.URN_WANIPConnection_1 {
				clients, err := internetgateway1.NewWANIPConnection1ClientsFromRootDevice(root, loc)
				if err != nil || len(clients) == 0 {
					return nil
				}
				clients[0].SOAPClient.HTTPClient = *hc
				return wanIPv1Wrapper{c: clients[0]}
			}
			clients, err := internetgateway1.NewWANPPPConnection1ClientsFromRootDevice(root, loc)
			if err != nil || len(clients) == 0 {
				return nil
			}
			clients[0].SOAPClient.HTTPClient = *hc
			return wanPPPv1Wrapper{c: clients[0]}
		})
}

// searchIGD searches for gateways offering any of the service targets from a
// socket fn controls, and returns the client build makes from the first that
// answers and describes itself. The description and every later SOAP call go
// over an HTTP client whose dials fn also controls. A search that finds no
// gateway returns nil and no error, as goupnp's does.
func searchIGD(ctx context.Context, fn control.Func, targets []string,
	build func(target string, root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient) (igdClient, error) {
	// Bound sockets bypass the tunnel, so every answer, and every address
	// later dialled, must be on the local network: a device there could
	// otherwise point the client at a host on the internet, which would then
	// see the user's own address.
	local, subnet, err := boundNetwork(fn)
	if err != nil {
		return nil, fmt.Errorf("local network: %w", err)
	}
	// Listening on the interface's own address, not just through fn, picks
	// the interface multicast leaves by where fn alone doesn't (Windows).
	lc := net.ListenConfig{Control: fn}
	pc, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort(local.String(), "0"))
	if err != nil {
		return nil, err
	}
	defer pc.Close()

	// Each sent three times, as goupnp does, since UDP can drop one.
	for range 3 {
		for _, target := range targets {
			req := []byte("M-SEARCH * HTTP/1.1\r\n" +
				"HOST: 239.255.255.250:1900\r\n" +
				"MAN: \"ssdp:discover\"\r\n" +
				"MX: 2\r\n" +
				"ST: " + target + "\r\n\r\n")
			if _, err := pc.WriteTo(req, ssdpAddr); err != nil {
				return nil, err
			}
		}
	}

	deadline := time.Now().Add(ssdpWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := pc.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = pc.SetReadDeadline(time.Now()) })
	defer stop()

	hc := boundHTTPClient(fn, subnet)
	found := false
	// The client keeps hc's connections; a search that returns none closes
	// them.
	defer func() {
		if !found {
			hc.CloseIdleConnections()
		}
	}()
	tried := map[string]bool{}
	var lastErr error
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// No gateway described itself in time; a failure to
				// describe one is worth reporting.
				return nil, lastErr
			}
			return nil, err
		}
		target, loc, ok := ssdpLocation(buf[:n], targets)
		key := target + " " + loc.String()
		if !ok || tried[key] || !sameLANHost(from, loc, subnet) {
			continue
		}
		tried[key] = true
		root, err := fetchRootDevice(ctx, hc, loc)
		if err != nil {
			lastErr = err
			continue
		}
		if c := build(target, root, loc, hc); c != nil && soapOnHost(c, loc) {
			found = true
			return c, nil
		}
	}
}

// ssdpLocation is which of targets an SSDP answer is for, and its LOCATION.
func ssdpLocation(b []byte, targets []string) (string, *url.URL, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return "", &url.URL{}, false
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &url.URL{}, false
	}
	st := resp.Header.Get("St")
	i := slices.IndexFunc(targets, func(t string) bool { return strings.EqualFold(st, t) })
	if i < 0 {
		return "", &url.URL{}, false
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host == "" {
		return "", &url.URL{}, false
	}
	return targets[i], loc, true
}

// sameLANHost reports whether an SSDP answer from from describes itself at
// from's own address, on subnet.
func sameLANHost(from net.Addr, loc *url.URL, subnet *net.IPNet) bool {
	ua, ok := from.(*net.UDPAddr)
	if !ok || !subnet.Contains(ua.IP) {
		return false
	}
	ip := net.ParseIP(loc.Hostname())
	return ip != nil && ip.Equal(ua.IP)
}

// soapOnHost reports whether c sends its SOAP calls to loc's host.
func soapOnHost(c igdClient, loc *url.URL) bool {
	var sc *soap.SOAPClient
	switch w := c.(type) {
	case wanIPv2Wrapper:
		sc = w.c.SOAPClient
	case wanIPv1Wrapper:
		sc = w.c.SOAPClient
	case wanPPPv1Wrapper:
		sc = w.c.SOAPClient
	}
	return sc != nil && sc.EndpointURL.Hostname() == loc.Hostname()
}

// boundHTTPClient is an HTTP client whose dials fn controls, never through a
// proxy and never following a redirect, and which dials only addresses on
// subnet: its sockets bypass the tunnel.
func boundHTTPClient(fn control.Func, subnet *net.IPNet) *http.Client {
	d := &net.Dialer{Control: fn, Timeout: deviceFetchTimeout}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip := net.ParseIP(host); ip == nil || !subnet.Contains(ip) {
					return nil, fmt.Errorf("refusing to dial %s off the local network", host)
				}
				return d.DialContext(ctx, network, addr)
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// fetchRootDevice is goupnp.DeviceByURLCtx over hc.
func fetchRootDevice(ctx context.Context, hc *http.Client, loc *url.URL) (*goupnp.RootDevice, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device description: status %s", resp.Status)
	}
	root := new(goupnp.RootDevice)
	dec := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DefaultSpace = goupnp.DeviceXMLNamespace
	dec.CharsetReader = goupnp.CharsetReaderDefault
	if err := dec.Decode(root); err != nil {
		return nil, fmt.Errorf("device description: %w", err)
	}
	// A URLBase on another host is ignored: the gateway is the host that
	// answered.
	base := loc
	if root.URLBaseStr != "" {
		if u, err := url.Parse(root.URLBaseStr); err == nil && u.Host == loc.Host {
			base = u
		}
	}
	root.SetURLBase(base)
	return root, nil
}
