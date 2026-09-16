// Package connectiondiag keeps bounded, local socket diagnostics for user reports.
package connectiondiag

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sagernet/sing-box/common/socketobserver"
	"github.com/sagernet/sing/common"
)

const capacity = 512

// Context installs the local collector for outbounds constructed with ctx.
func Context(ctx context.Context) context.Context {
	return socketobserver.WithObserver(ctx, observer{})
}

type observer struct{}

func (observer) Begin(label socketobserver.Label, network, endpoint string) func(net.Conn, error) net.Conn {
	return Begin(label, network, endpoint)
}
func (observer) BeginPacket(label socketobserver.Label, endpoint string) func(error) {
	return BeginPacket(label, endpoint)
}

// TCPStats contains optional measurements in explicit units; nil fields are unavailable.
type TCPStats struct {
	Source               string  `json:"source"`
	State                uint32  `json:"state"`
	RTTUS                *uint64 `json:"rtt_us,omitempty"`
	RetransmittedPackets *uint64 `json:"retransmitted_packets,omitempty"`
	RetransmittedBytes   *uint64 `json:"retransmitted_bytes,omitempty"`
	SentBytes            *uint64 `json:"sent_bytes,omitempty"`
	ReceivedBytes        *uint64 `json:"received_bytes,omitempty"`
}

// Record describes a physical dial, not a logical stream or an authenticated handshake.
// Observed I/O fields exclude raw-socket operations and TCP Fast Open initial payloads.
type Record struct {
	DialGroup  uint64    `json:"dial_group,omitempty"`
	ID         uint64    `json:"id"`
	Started    time.Time `json:"started"`
	Protocol   string    `json:"protocol"`
	OutboundID string    `json:"outbound_id"`
	EndpointID string    `json:"endpoint_id"`
	Network    string    `json:"network"`
	DialMS     int64     `json:"dial_ms"`
	DialDone   bool      `json:"dial_done"`
	Error      string    `json:"error,omitempty"`
	ErrorStage string    `json:"error_stage,omitempty"`
	Closed     bool      `json:"closed"`
	// FirstReadMS and observed byte counts exclude splice, raw I/O, and TFO initial payloads.
	// Zero counts do not establish that no traffic flowed.
	FirstReadMS   *int64     `json:"first_observed_read_ms,omitempty"`
	ReadBytes     uint64     `json:"observed_read_bytes"`
	WrittenBytes  uint64     `json:"observed_written_bytes"`
	IOCoverage    string     `json:"io_coverage"`
	TCP           *TCPStats  `json:"tcp,omitempty"`
	TCPStatus     string     `json:"tcp_status"`
	TCPObservedAt *time.Time `json:"tcp_observed_at,omitempty"`
}
type entry struct {
	sync.Mutex
	record       Record
	started      time.Time
	socket       net.Conn
	firstRead    atomic.Int64
	readBytes    atomic.Uint64
	writtenBytes atomic.Uint64
}

var history struct {
	sync.Mutex
	entries [capacity]*entry
	next    int
	total   uint64
	key     [32]byte
}
var (
	enabled       atomic.Bool
	includeDirect atomic.Bool
	initKey       sync.Once
)

// Enable starts in-memory collection; direct outbounds are included only when requested.
func Enable(direct bool) {
	history.Lock()
	defer history.Unlock()
	initKey.Do(func() {
		history.key = [32]byte{}
		if _, err := rand.Read(history.key[:]); err != nil {
			panic(err)
		}
	})
	includeDirect.Store(direct)
	enabled.Store(true)
}

// Disable stops new collection and discards retained records without closing connections.
// Completion hooks already in flight may finish but cannot reinsert discarded records.
func Disable() {
	history.Lock()
	defer history.Unlock()
	enabled.Store(false)
	history.entries = [capacity]*entry{}
	history.next = 0
	history.total = 0
}

// OutboundID returns the process-local identifier used in snapshots, or empty when disabled.
// Identifiers cannot be correlated across process restarts.
func OutboundID(tag string) string {
	history.Lock()
	defer history.Unlock()
	if !enabled.Load() {
		return ""
	}
	return digest(tag)
}

func digest(value string) string {
	h := hmac.New(sha256.New, history.key[:])
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// Begin records one physical dial and returns its completion hook; nil means this dial is excluded.
func Begin(label socketobserver.Label, network, endpoint string) func(net.Conn, error) net.Conn {
	e := newEntry(label, network, endpoint)
	if e == nil {
		return nil
	}
	now := e.started
	return func(c net.Conn, err error) net.Conn {
		e.Lock()
		e.record.DialDone = true
		e.record.DialMS = time.Since(now).Milliseconds()
		if err != nil {
			e.record.Error = errorClass(err)
			e.record.ErrorStage = "connect"
			e.Unlock()
			return c
		}
		e.Unlock()
		if c == nil {
			return c
		}
		e.Lock()
		e.socket = c
		e.Unlock()
		e.sample(c)
		return &conn{Conn: c, e: e}
	}
}

func newEntry(label socketobserver.Label, network, endpoint string) *entry {
	if !enabled.Load() || label.Protocol == "" || (label.Protocol == "direct" && !includeDirect.Load()) {
		return nil
	}
	history.Lock()
	defer history.Unlock()
	if !enabled.Load() || (label.Protocol == "direct" && !includeDirect.Load()) {
		return nil
	}
	now := time.Now()
	e := &entry{started: now, record: Record{DialGroup: label.DialGroup, IOCoverage: "partial", Started: now.UTC(), Protocol: label.Protocol, OutboundID: digest(label.Tag), EndpointID: digest(endpoint), Network: network, TCPStatus: "not_connected"}}
	history.total++
	e.record.ID = history.total
	history.entries[history.next] = e
	history.next = (history.next + 1) % capacity
	return e
}

// BeginPacket records UDP socket setup, which does not establish peer reachability.
func BeginPacket(label socketobserver.Label, endpoint string) func(error) {
	e := newEntry(label, "udp", endpoint)
	if e == nil {
		return nil
	}
	return func(err error) {
		e.Lock()
		defer e.Unlock()
		e.record.DialDone = true
		e.record.DialMS = time.Since(e.started).Milliseconds()
		e.record.TCPStatus = "not_tcp"
		if err != nil {
			e.record.Error = errorClass(err)
			e.record.ErrorStage = "socket_setup"
		}
	}
}

func errorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, io.EOF):
		return "eof"
	}
	if class := socketErrorClass(err); class != "" {
		return class
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "other"
}

func (e *entry) sample(c net.Conn) {
	e.Lock()
	network := e.record.Network
	e.Unlock()
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		e.Lock()
		e.record.TCPStatus = "not_tcp"
		e.Unlock()
		return
	}
	s, status := socketStats(c)
	e.Lock()
	defer e.Unlock()
	if s != nil {
		e.record.TCP = s
		observed := time.Now().UTC()
		e.record.TCPObservedAt = &observed
		e.record.TCPStatus = "available"
	} else if e.record.TCP == nil {
		e.record.TCPStatus = status
	}
}

func socketStats(c net.Conn) (*TCPStats, string) {
	sc, ok := common.Cast[syscall.Conn](c)
	if !ok {
		return nil, "no_socket_handle"
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, "socket_unavailable"
	}
	var result *TCPStats
	var statErr error
	err = raw.Control(func(fd uintptr) { result, statErr = readTCPStats(fd) })
	if err != nil || statErr != nil {
		return nil, "unsupported_or_unavailable"
	}
	return result, "available"
}

type conn struct {
	net.Conn
	e    *entry
	once sync.Once
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.e.readBytes.Add(uint64(n))
		c.e.firstRead.CompareAndSwap(0, time.Since(c.e.started).Nanoseconds()+1)
	}
	c.failure("read", err)
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.e.writtenBytes.Add(uint64(n))
	}
	c.failure("write", err)
	return n, err
}

func (c *conn) failure(stage string, err error) {
	if err == nil {
		return
	}
	c.e.sample(c.Conn)
	c.e.Lock()
	if c.e.record.Error == "" {
		c.e.record.Error = errorClass(err)
		c.e.record.ErrorStage = stage
	}
	c.e.Unlock()
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.e.sample(c.Conn)
		c.e.Lock()
		c.e.record.Closed = true
		c.e.socket = nil
		c.e.Unlock()
	})
	return c.Conn.Close()
}

// SyscallConn forwards socket access; raw I/O bypasses the observed I/O fields.
func (c *conn) SyscallConn() (syscall.RawConn, error) {
	sc, ok := common.Cast[syscall.Conn](c.Conn)
	if !ok {
		return nil, syscall.EINVAL
	}
	return sc.SyscallConn()
}

// NeedHandshake preserves lazy transport handshake signaling.
func (c *conn) NeedHandshake() bool {
	h, ok := common.Cast[interface{ NeedHandshake() bool }](c.Conn)
	return ok && h.NeedHandshake()
}

// Upstream preserves access to socket capabilities through this wrapper.
func (c *conn) Upstream() any { return c.Conn }

// ReaderReplaceable retains this wrapper during reader unwrapping.
func (c *conn) ReaderReplaceable() bool { return false }

// WriterReplaceable retains this wrapper during writer unwrapping.
func (c *conn) WriterReplaceable() bool { return false }

// Snapshot returns recent records without addresses, payloads, or raw error strings.
func Snapshot() ([]byte, error) {
	history.Lock()
	entries := make([]*entry, 0, capacity)
	for i := range capacity {
		if e := history.entries[(history.next+i)%capacity]; e != nil {
			entries = append(entries, e)
		}
	}
	total := history.total
	collecting := enabled.Load()
	history.Unlock()
	records := make([]Record, 0, len(entries))
	for _, e := range entries {
		e.Lock()
		socket := e.socket
		e.Unlock()
		if socket != nil {
			e.sample(socket)
		}
		e.Lock()
		r := e.record
		e.Unlock()
		r.ReadBytes = e.readBytes.Load()
		r.WrittenBytes = e.writtenBytes.Load()
		if first := e.firstRead.Load(); first != 0 {
			ms := (first - 1) / int64(time.Millisecond)
			r.FirstReadMS = &ms
		}
		records = append(records, r)
	}
	return json.Marshal(struct {
		Version int      `json:"version"`
		Enabled bool     `json:"enabled"`
		Total   uint64   `json:"total_dials"`
		Evicted uint64   `json:"evicted"`
		Records []Record `json:"records"`
	}{1, collecting, total, total - uint64(len(entries)), records})
}
func u64(v uint64) *uint64 { return &v }
