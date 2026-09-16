//nolint:paralleltest // Tests share the process-wide diagnostic recorder.
package connectiondiag

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/socketobserver"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
)

func resetHistory() {
	history.Lock()
	history.entries = [capacity]*entry{}
	history.total = 0
	history.next = 0
	history.Unlock()
	Enable(false)
}

func TestBoundedPrivateHistory(t *testing.T) {
	resetHistory()
	for range capacity + 9 {
		done := Begin(socketobserver.Label{Protocol: "twiddle", Tag: "secret-route"}, "tcp", "secret.example:443")
		done(nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded})
	}
	data, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret")) {
		t.Fatal("identity exposed")
	}
	var v struct {
		Total   uint64   `json:"total_dials"`
		Evicted uint64   `json:"evicted"`
		Records []Record `json:"records"`
	}
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Records) != capacity || v.Total != capacity+9 || v.Evicted != 9 {
		t.Fatalf("wrong bounds: %d %d %d", len(v.Records), v.Total, v.Evicted)
	}
	if v.Records[0].ID != 10 || v.Records[0].Error != "timeout" || v.Records[0].ErrorStage != "connect" {
		t.Fatalf("wrong record: %+v", v.Records[0])
	}
	if Begin(socketobserver.Label{Protocol: "direct", Tag: "direct"}, "tcp", "example.org:443") != nil {
		t.Fatal("direct collected without opt-in")
	}
}

func TestSocketLifecycle(t *testing.T) {
	resetHistory()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			accepted <- e
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, e = io.CopyN(c, c, 4)
		accepted <- e
	}()
	done := Begin(socketobserver.Label{Protocol: "test", Tag: "test-route"}, "tcp", ln.Addr().String())
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	c = done(c, err)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := N.CastReader[interface {
		io.Reader
		syscall.Conn
	}](c); !ok {
		t.Fatal("socket capability hidden from kernel TLS")
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = io.Copy(c, strings.NewReader("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err = io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
	data, _ := Snapshot()
	var v struct {
		Records []Record `json:"records"`
	}
	json.Unmarshal(data, &v)
	r := v.Records[0]
	if !r.Closed || !r.DialDone || r.ReadBytes != 4 || r.WrittenBytes != 4 || r.FirstReadMS == nil {
		t.Fatalf("missing lifecycle data: %+v", r)
	}
	if r.TCP == nil || r.TCPStatus != "available" {
		t.Fatalf("socket statistics unavailable: %+v", r)
	}
}

func TestNoSocketAndConcurrentSnapshot(t *testing.T) {
	resetHistory()
	a, b := net.Pipe()
	defer b.Close()
	c := Begin(socketobserver.Label{Protocol: "test", Tag: "route"}, "tcp", "hidden:443")(a, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			Snapshot()
		}
	}()
	c.Close()
	<-done
	data, _ := Snapshot()
	if !bytes.Contains(data, []byte("no_socket_handle")) {
		t.Fatalf("unavailable not explicit: %s", data)
	}
}

func TestVectorisedIOKeepsSocketStatistics(t *testing.T) {
	resetHistory()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	finished := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			finished <- e
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, e = io.CopyN(c, c, 4)
		finished <- e
	}()
	done := Begin(socketobserver.Label{Protocol: "test", Tag: "vectorised"}, "tcp", ln.Addr().String())
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	c = done(c, err)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	err = bufio.NewVectorisedWriter(c).WriteVectorised([]*buf.Buffer{buf.As([]byte("pi")), buf.As([]byte("ng"))})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err = io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	data, _ := Snapshot()
	var v struct {
		Records []Record `json:"records"`
	}
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	r := v.Records[0]
	if r.TCP == nil || r.TCPObservedAt == nil || r.ReadBytes != 4 || string(b) != "ping" {
		t.Fatalf("vectorized path failed: %+v %q", r, b)
	}
	c.Close()
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
}
