package portforward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/sagernet/sing/common/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingControl is a socket control that counts the sockets it sees.
func countingControl() (control.Func, *atomic.Int32) {
	var n atomic.Int32
	return func(string, string, syscall.RawConn) error {
		n.Add(1)
		return nil
	}, &n
}

func stubGatewayLookup(t *testing.T, local string, addrs []string, gws []string) *atomic.Int32 {
	t.Helper()
	prevLocal, prevAddrs, prevGws, prevFind := boundLocalIP, interfaceAddrs, discoverGateways, findGateway
	t.Cleanup(func() {
		boundLocalIP, interfaceAddrs, discoverGateways, findGateway = prevLocal, prevAddrs, prevGws, prevFind
	})
	boundLocalIP = func(control.Func) (string, error) { return local, nil }
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, a := range addrs {
			ip, n, err := net.ParseCIDR(a)
			require.NoError(t, err)
			out = append(out, &net.IPNet{IP: ip, Mask: n.Mask})
		}
		return out, nil
	}
	discoverGateways = func() ([]net.IP, error) {
		var out []net.IP
		for _, g := range gws {
			out = append(out, net.ParseIP(g))
		}
		return out, nil
	}
	var fallbacks atomic.Int32
	findGateway = func(context.Context) (net.IP, error) {
		fallbacks.Add(1)
		return net.ParseIP("10.10.1.2"), nil
	}
	return &fallbacks
}

// While a tunnel's gateway comes first in the routing table, a bound lookup
// still finds the router on the physical interface's subnet.
func TestFindGatewayBound_PicksThePhysicalSubnet(t *testing.T) {
	fallbacks := stubGatewayLookup(t, "192.168.1.20",
		[]string{"10.10.1.1/30", "192.168.1.20/24"},
		[]string{"10.10.1.2", "192.168.1.1"})
	fn, _ := countingControl()

	ip, err := findGatewayBound(context.Background(), fn)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.1", ip.String())
	assert.Zero(t, fallbacks.Load())
}

// With no gateway on the bound subnet, a bound lookup fails rather than hand
// back a gateway off the local network, which bound requests would reach
// outside the tunnel.
func TestFindGatewayBound_FailsWithoutAGatewayOnTheSubnet(t *testing.T) {
	fallbacks := stubGatewayLookup(t, "192.168.1.20",
		[]string{"192.168.1.20/24"}, []string{"10.10.1.2"})
	fn, _ := countingControl()

	_, err := findGatewayBound(context.Background(), fn)
	assert.ErrorContains(t, err, "no gateway in 192.168.1.0/24")
	assert.Zero(t, fallbacks.Load())
}

func TestFindGatewayBound_UnboundIsFindGateway(t *testing.T) {
	fallbacks := stubGatewayLookup(t, "192.168.1.20",
		[]string{"192.168.1.20/24"}, []string{"192.168.1.1"})

	ip, err := findGatewayBound(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "10.10.1.2", ip.String())
	assert.Equal(t, int32(1), fallbacks.Load())
}

// Discovery and every later request to a PCP gateway go through the control.
func TestDiscoverPMPBound_UsesTheControl(t *testing.T) {
	g := newFakePMPGateway(t, true)
	fn, n := countingControl()

	c, method, err := discoverPMPBound(context.Background(), g.addr(), fn)
	require.NoError(t, err)
	assert.Equal(t, "pcp", method)
	afterDiscovery := n.Load()
	assert.Positive(t, afterDiscovery, "the probes' sockets")

	require.NoError(t, c.AddPortMapping("", 30001, TCP, 30001, "", true, "test", 3600))
	assert.Greater(t, n.Load(), afterDiscovery, "the mapping request's socket")
}

func TestNewForwarder_BoundToSetsTheLocalIPControl(t *testing.T) {
	fn, _ := countingControl()
	f := newForwarder(&fakeIGD{}, "test", applyOptions([]Option{BoundTo(fn)}))
	assert.NotNil(t, f.localIPControl)
	assert.Nil(t, newForwarder(&fakeIGD{}, "test", applyOptions(nil)).localIPControl)
}

const testDeviceXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:2</deviceType>
    <friendlyName>test gateway</friendlyName>
    <deviceList><device>
      <deviceType>urn:schemas-upnp-org:device:WANDevice:2</deviceType>
      <deviceList><device>
        <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:2</deviceType>
        <serviceList><service>
          <serviceType>urn:schemas-upnp-org:service:WANIPConnection:2</serviceType>
          <serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
          <controlURL>/ctl</controlURL>
          <eventSubURL>/evt</eventSubURL>
          <SCPDURL>/scpd.xml</SCPDURL>
        </service></serviceList>
      </device></deviceList>
    </device></deviceList>
  </device>
</root>`

const testExternalIPResponse = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:2">
<NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>
</u:GetExternalIPAddressResponse></s:Body></s:Envelope>`

// loopbackIsTheLAN makes 127.0.0.1 the bound address and 127.0.0.0/8 the
// local network, where the fakes below run.
func loopbackIsTheLAN(t *testing.T) {
	t.Helper()
	prevLocal, prevAddrs := boundLocalIP, interfaceAddrs
	t.Cleanup(func() { boundLocalIP, interfaceAddrs = prevLocal, prevAddrs })
	boundLocalIP = func(control.Func) (string, error) { return "127.0.0.1", nil }
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)}}, nil
	}
}

// fakeSSDP answers SSDP searches for target once for each location, in
// order, on loopback, and points ssdpAddr at itself.
func fakeSSDP(t *testing.T, target string, locations ...string) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	prev := ssdpAddr
	ssdpAddr = conn.LocalAddr().(*net.UDPAddr)
	t.Cleanup(func() { ssdpAddr = prev })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if !strings.Contains(string(buf[:n]), "ST: "+target) {
				continue
			}
			for _, location := range locations {
				resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: %s\r\nUSN: uuid:test::%s\r\nLOCATION: %s\r\n\r\n", target, target, location)
				_, _ = conn.WriteTo([]byte(resp), from)
			}
		}
	}()
}

// fakeIGDServer serves the device description at /desc.xml, with its
// URLBase set to urlBase if not empty, and answers GetExternalIPAddress.
func fakeIGDServer(t *testing.T, urlBase string) *httptest.Server {
	t.Helper()
	desc := testDeviceXML
	if urlBase != "" {
		desc = strings.Replace(desc, "<device>", "<URLBase>"+urlBase+"</URLBase><device>", 1)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/desc.xml":
			_, _ = w.Write([]byte(desc))
		case "/ctl":
			w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
			_, _ = w.Write([]byte(testExternalIPResponse))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A bound UPnP search finds the gateway, and its description and SOAP calls
// go over sockets the control sees.
func TestDiscoverIGDv2Bound_UsesTheControl(t *testing.T) {
	loopbackIsTheLAN(t)
	srv := fakeIGDServer(t, "")
	fakeSSDP(t, internetgateway2.URN_WANIPConnection_2, srv.URL+"/desc.xml")
	fn, n := countingControl()

	c, err := discoverIGDv2Bound(context.Background(), fn)
	require.NoError(t, err)
	require.NotNil(t, c)
	afterDiscovery := n.Load()
	assert.GreaterOrEqual(t, afterDiscovery, int32(2), "the search socket and the description fetch")

	ip, err := c.GetExternalIPAddress()
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)
	assert.GreaterOrEqual(t, n.Load(), afterDiscovery, "SOAP calls go over the bound client")
}

// With no gateway answering, the search ends at ssdpWait with no client and
// no error, as goupnp's does.
func TestSearchIGD_NoAnswer(t *testing.T) {
	loopbackIsTheLAN(t)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	prevAddr, prevWait := ssdpAddr, ssdpWait
	ssdpAddr, ssdpWait = conn.LocalAddr().(*net.UDPAddr), 200*time.Millisecond
	t.Cleanup(func() { ssdpAddr, ssdpWait = prevAddr, prevWait })
	fn, _ := countingControl()

	start := time.Now()
	c, err := discoverIGDv2Bound(context.Background(), fn)
	assert.NoError(t, err)
	assert.Nil(t, c)
	assert.Less(t, time.Since(start), 2*time.Second)
}

// A control that refuses the socket fails the search rather than falling
// back to an unbound one.
func TestSearchIGD_ControlErrorFails(t *testing.T) {
	refuse := func(string, string, syscall.RawConn) error { return errors.New("no physical interface") }
	_, err := discoverIGDv2Bound(context.Background(), refuse)
	assert.ErrorContains(t, err, "no physical interface")
}

// An answer whose LOCATION is on another host than the one that sent it is
// ignored: bound requests bypass the tunnel, so the client must not be sent
// anywhere else.
func TestSearchIGD_IgnoresALocationOnAnotherHost(t *testing.T) {
	loopbackIsTheLAN(t)
	prevWait := ssdpWait
	ssdpWait = 300 * time.Millisecond
	t.Cleanup(func() { ssdpWait = prevWait })
	srv := fakeIGDServer(t, "")
	// Reachable, so only the check refuses it.
	elsewhere := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	fakeSSDP(t, internetgateway2.URN_WANIPConnection_2, elsewhere+"/desc.xml")
	fn, _ := countingControl()

	c, err := discoverIGDv2Bound(context.Background(), fn)
	assert.NoError(t, err)
	assert.Nil(t, c)
}

// A URLBase naming another host is ignored, so SOAP calls still go to the
// gateway that answered.
func TestSearchIGD_IgnoresAURLBaseOnAnotherHost(t *testing.T) {
	loopbackIsTheLAN(t)
	srv := fakeIGDServer(t, "http://203.0.113.9:80/")
	fakeSSDP(t, internetgateway2.URN_WANIPConnection_2, srv.URL+"/desc.xml")
	fn, _ := countingControl()

	c, err := discoverIGDv2Bound(context.Background(), fn)
	require.NoError(t, err)
	require.NotNil(t, c)
	ip, err := c.GetExternalIPAddress()
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)
}

// The bound HTTP client dials nothing off the local network and follows no
// redirect.
func TestBoundHTTPClient_StaysOnTheLAN(t *testing.T) {
	subnet := &net.IPNet{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}
	hc := boundHTTPClient(nil, subnet)

	_, err := hc.Get("http://203.0.113.9:80/desc.xml")
	assert.ErrorContains(t, err, "off the local network")

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.9/", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	resp, err := hc.Get(redirect.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode, "the redirect is not followed")
}

// IGDv1's two connection services are searched for at once, and a gateway
// offering only WANPPPConnection is found.
func TestDiscoverIGDv1Bound_FindsWANPPP(t *testing.T) {
	loopbackIsTheLAN(t)
	desc := strings.NewReplacer(
		"InternetGatewayDevice:2", "InternetGatewayDevice:1",
		"WANDevice:2", "WANDevice:1",
		"WANConnectionDevice:2", "WANConnectionDevice:1",
		"service:WANIPConnection:2", "service:WANPPPConnection:1",
		"WANIPConn1", "WANPPPConn1").Replace(testDeviceXML)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/desc.xml" {
			_, _ = w.Write([]byte(desc))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	fakeSSDP(t, "urn:schemas-upnp-org:service:WANPPPConnection:1", srv.URL+"/desc.xml")
	fn, _ := countingControl()

	start := time.Now()
	c, err := discoverIGDv1Bound(context.Background(), fn)
	require.NoError(t, err)
	assert.IsType(t, wanPPPv1Wrapper{}, c)
	assert.Less(t, time.Since(start), ssdpWait, "found without waiting out the search")
}

// A device that answers first and is slow to describe itself doesn't keep a
// working gateway's answer from being read and used.
func TestSearchIGD_SlowResponderDoesNotHideAGateway(t *testing.T) {
	loopbackIsTheLAN(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); slow.Close() })
	srv := fakeIGDServer(t, "")
	fakeSSDP(t, internetgateway2.URN_WANIPConnection_2, slow.URL+"/desc.xml", srv.URL+"/desc.xml")
	fn, _ := countingControl()

	start := time.Now()
	c, err := discoverIGDv2Bound(context.Background(), fn)
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Less(t, time.Since(start), deviceFetchTimeout, "found without waiting on the slow device")
}
