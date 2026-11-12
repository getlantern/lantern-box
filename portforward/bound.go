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
	"strings"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
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
// the caller's ctx. Gateways answer within the request's MX of 2 seconds.
var ssdpWait = 3 * time.Second

// deviceFetchTimeout bounds fetching one gateway's device description, as
// goupnp does.
const deviceFetchTimeout = 3 * time.Second

// discoverIGDv2Bound is discoverIGDv2 through sockets fn controls.
func discoverIGDv2Bound(ctx context.Context, fn control.Func) (igdClient, error) {
	return searchIGD(ctx, fn, internetgateway2.URN_WANIPConnection_2,
		func(root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient {
			clients, err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, loc)
			if err != nil || len(clients) == 0 {
				return nil
			}
			clients[0].SOAPClient.HTTPClient = *hc
			return wanIPv2Wrapper{c: clients[0]}
		})
}

// discoverIGDv1Bound is discoverIGDv1 through sockets fn controls.
func discoverIGDv1Bound(ctx context.Context, fn control.Func) (igdClient, error) {
	c, err := searchIGD(ctx, fn, internetgateway1.URN_WANIPConnection_1,
		func(root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient {
			clients, err := internetgateway1.NewWANIPConnection1ClientsFromRootDevice(root, loc)
			if err != nil || len(clients) == 0 {
				return nil
			}
			clients[0].SOAPClient.HTTPClient = *hc
			return wanIPv1Wrapper{c: clients[0]}
		})
	if err == nil && c != nil {
		return c, nil
	}
	return searchIGD(ctx, fn, internetgateway1.URN_WANPPPConnection_1,
		func(root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient {
			clients, err := internetgateway1.NewWANPPPConnection1ClientsFromRootDevice(root, loc)
			if err != nil || len(clients) == 0 {
				return nil
			}
			clients[0].SOAPClient.HTTPClient = *hc
			return wanPPPv1Wrapper{c: clients[0]}
		})
}

// searchIGD searches for gateways offering the service target from a socket
// fn controls, and returns the client build makes from the first that
// answers and describes itself. The description and every later SOAP call go
// over an HTTP client whose dials fn also controls. A search that finds no
// gateway returns nil and no error, as goupnp's does.
func searchIGD(ctx context.Context, fn control.Func, target string,
	build func(root *goupnp.RootDevice, loc *url.URL, hc *http.Client) igdClient) (igdClient, error) {
	lc := net.ListenConfig{Control: fn}
	pc, err := lc.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()

	req := []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + target + "\r\n\r\n")
	// Sent three times, as goupnp does, since UDP can drop one.
	for range 3 {
		if _, err := pc.WriteTo(req, ssdpAddr); err != nil {
			return nil, err
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

	hc := boundHTTPClient(fn)
	tried := map[string]bool{}
	var lastErr error
	buf := make([]byte, 2048)
	for {
		n, _, err := pc.ReadFrom(buf)
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
		loc, ok := ssdpLocation(buf[:n], target)
		if !ok || tried[loc.String()] {
			continue
		}
		tried[loc.String()] = true
		root, err := fetchRootDevice(ctx, hc, loc)
		if err != nil {
			lastErr = err
			continue
		}
		if c := build(root, loc, hc); c != nil {
			return c, nil
		}
	}
}

// ssdpLocation is the LOCATION of an SSDP answer for target.
func ssdpLocation(b []byte, target string) (*url.URL, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return nil, false
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.EqualFold(resp.Header.Get("St"), target) {
		return nil, false
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Host == "" {
		return nil, false
	}
	return loc, true
}

// boundHTTPClient is an HTTP client whose dials fn controls, never through a
// proxy: the gateway is on the local network.
func boundHTTPClient(fn control.Func) *http.Client {
	d := &net.Dialer{Control: fn, Timeout: deviceFetchTimeout}
	return &http.Client{Transport: &http.Transport{
		DialContext:     d.DialContext,
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}}
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
	base := loc
	if root.URLBaseStr != "" {
		if base, err = url.Parse(root.URLBaseStr); err != nil {
			return nil, fmt.Errorf("device description: URLBase: %w", err)
		}
	}
	root.SetURLBase(base)
	return root, nil
}
