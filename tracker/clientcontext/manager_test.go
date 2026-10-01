package clientcontext

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

const (
	testInfoJSON = `{"DeviceID":"test-device","Platform":"linux"}`
	testRequest  = "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
)

var testInfo = ClientInfo{DeviceID: "test-device", Platform: "linux"}

// scriptedRead is one Read result for scriptedConn.
type scriptedRead struct {
	data string
	err  error
}

// scriptedConn returns scripted reads in order, then io.EOF, and records
// writes. It ignores deadlines, so tests can place a read timeout exactly.
type scriptedConn struct {
	net.Conn
	reads   []scriptedRead
	written bytes.Buffer
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}
	r := c.reads[0]
	n := copy(p, r.data)
	if n < len(r.data) {
		c.reads[0].data = r.data[n:]
		return n, nil
	}
	c.reads = c.reads[1:]
	return n, r.err
}

func (c *scriptedConn) Write(p []byte) (int, error)     { return c.written.Write(p) }
func (c *scriptedConn) SetReadDeadline(time.Time) error { return nil }

func newScriptedReadConn(reads ...scriptedRead) (*readConn, *scriptedConn) {
	conn := &scriptedConn{reads: reads}
	return &readConn{Conn: conn, reader: conn}, conn
}

// readInfoWithin runs c.readInfo and fails the test if it blocks.
func readInfoWithin(t *testing.T, c *readConn) (ClientInfo, error) {
	t.Helper()
	type result struct {
		info ClientInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := c.readInfo()
		done <- result{info, err}
	}()
	select {
	case r := <-done:
		return r.info, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("readInfo blocked")
		return ClientInfo{}, nil
	}
}

func setReadInfoTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := readInfoTimeout
	readInfoTimeout = d
	t.Cleanup(func() { readInfoTimeout = old })
}

// Whatever follows a frame, or the whole stream when there is none, must reach
// the destination unaltered.
func TestReadInfo(t *testing.T) {
	tests := []struct {
		name     string
		stream   string
		wantInfo ClientInfo
		wantAck  string
		wantRest string
	}{
		{name: "frame", stream: clientInfoPrefix + testInfoJSON + testRequest, wantInfo: testInfo, wantRest: testRequest},
		{name: "legacy frame", stream: legacyClientInfoPrefix + testInfoJSON, wantInfo: testInfo, wantAck: ackResponse},
		{name: "empty info", stream: clientInfoPrefix + "{}" + testRequest, wantRest: testRequest},
		{name: "no frame", stream: testRequest, wantRest: testRequest},
		{name: "short", stream: "hi", wantRest: "hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, scripted := newScriptedReadConn(scriptedRead{data: tt.stream})

			info, err := conn.readInfo()
			require.NoError(t, err)
			assert.Equal(t, tt.wantInfo, info)
			assert.Equal(t, tt.wantAck, scripted.written.String())

			rest, err := io.ReadAll(conn)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRest, string(rest))
		})
	}
}

func TestReadInfoErrors(t *testing.T) {
	// The marker and part of the frame are already consumed, so the connection
	// must fail rather than forward a truncated stream.
	t.Run("malformed", func(t *testing.T) {
		conn, _ := newScriptedReadConn(scriptedRead{data: clientInfoPrefix + "{not valid json"})

		info, err := conn.readInfo()
		require.Error(t, err)
		require.Zero(t, info)
		_, err = conn.Read(make([]byte, 8))
		require.Error(t, err, "later reads must return the failure")
	})

	// A conn that dies before sending anything is not a client-info failure, so
	// it is not reported, but its later reads still fail.
	t.Run("dead conn", func(t *testing.T) {
		conn, _ := newScriptedReadConn()

		info, err := conn.readInfo()
		require.NoError(t, err)
		require.Zero(t, info)
		_, err = conn.Read(make([]byte, 8))
		require.ErrorIs(t, err, io.EOF)
	})
}

// TCP can split the marker across reads, so the stream must be classified from
// accumulated bytes rather than a single Read.
func TestReadInfoSplit(t *testing.T) {
	packet := clientInfoPrefix + testInfoJSON
	for _, chunk := range []int{1, 5, 10, 11, 32} {
		t.Run(fmt.Sprintf("%d-byte", chunk), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			// net.Pipe is unbuffered, so each Write lands as its own Read.
			go func() {
				for off := 0; off < len(packet); off += chunk {
					if _, err := client.Write([]byte(packet[off:min(off+chunk, len(packet))])); err != nil {
						return
					}
				}
			}()

			info, err := readInfoWithin(t, &readConn{Conn: server, reader: server})
			require.NoError(t, err)
			assert.Equal(t, testInfo, info)
		})
	}
}

// The classification read is bounded, so a client that waits for the server
// cannot stall the connection. A frame that arrives after the deadline, whole or
// with its marker split across it, is still kept from the destination, and an
// opening that cannot start a frame is decided without waiting.
func TestReadInfoTimeout(t *testing.T) {
	const payload = "USER alice\r\n"
	tests := []struct {
		name    string
		timeout time.Duration
		before  string // sent before readInfo returns
		after   string // sent once readInfo returns
		wantAck string
		want    string // what reaches the destination
	}{
		{name: "short", timeout: time.Hour, before: "hi", after: payload, want: "hi" + payload},
		// A server-first protocol (SMTP, IMAP, SSH) sends nothing until greeted.
		{name: "silent", after: payload, want: payload},
		{name: "late frame", after: clientInfoPrefix + testInfoJSON + payload, want: payload},
		{name: "split marker", before: "CLIENT", after: "INFO2 " + testInfoJSON + payload, want: payload},
		{name: "late legacy frame", after: legacyClientInfoPrefix + testInfoJSON + payload, wantAck: ackResponse, want: payload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setReadInfoTimeout(t, cmp.Or(tt.timeout, 50*time.Millisecond))
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			acks := make(chan string, 1)
			go func() {
				buf := make([]byte, 16)
				n, _ := client.Read(buf)
				acks <- string(buf[:n])
			}()
			if tt.before != "" {
				go client.Write([]byte(tt.before))
			}

			c := &readConn{Conn: server, reader: server}
			info, err := readInfoWithin(t, c)
			require.NoError(t, err)
			require.Zero(t, info, "a late frame is too late to attribute")
			require.NoError(t, c.readErr, "the timeout must not fail later reads")

			go client.Write([]byte(tt.after))
			got := make([]byte, len(tt.want))
			_, err = io.ReadFull(c, got)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
			if tt.wantAck != "" {
				select {
				case ack := <-acks:
					assert.Equal(t, tt.wantAck, ack)
				case <-time.After(5 * time.Second):
					t.Fatal("no ack")
				}
			}
		})
	}
}

// readRetryingTimeouts reads r to io.EOF, retrying after read timeouts as a
// caller that resets its deadline would.
func readRetryingTimeouts(t *testing.T, r io.Reader) string {
	t.Helper()
	var got []byte
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		switch {
		case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
		case errors.Is(err, io.EOF):
			return string(got)
		default:
			require.NoError(t, err)
		}
	}
}

// A client that has sent part of a marker is mid-frame, so it gets a second
// window to finish: the frame is still attributed, and a legacy sender is
// acked before routing can send it destination bytes. Past that, the frame is
// still stripped, including when a later read times out mid-marker.
func TestReadInfoDeadline(t *testing.T) {
	timeout := scriptedRead{err: os.ErrDeadlineExceeded}
	tests := []struct {
		name     string
		reads    []scriptedRead
		wantInfo ClientInfo
		wantAck  string
	}{
		{
			name:     "second window",
			reads:    []scriptedRead{{data: "CLIENT"}, timeout, {data: "INFO2 " + testInfoJSON + testRequest}},
			wantInfo: testInfo,
		},
		{
			name:     "legacy second window",
			reads:    []scriptedRead{{data: "CLIENT"}, timeout, {data: "INFO " + testInfoJSON + testRequest}},
			wantInfo: testInfo,
			wantAck:  ackResponse,
		},
		{
			name:  "past second window",
			reads: []scriptedRead{{data: "CLIENT"}, timeout, timeout, {data: "INFO2 " + testInfoJSON + testRequest}},
		},
		{
			name:  "timeout while pending",
			reads: []scriptedRead{timeout, {data: "CLIENT"}, timeout, {data: "INFO2 " + testInfoJSON + testRequest}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, conn := newScriptedReadConn(tt.reads...)

			info, err := c.readInfo()
			require.NoError(t, err)
			assert.Equal(t, tt.wantInfo, info)
			assert.Equal(t, tt.wantAck, conn.written.String(), "the ack must precede routing")
			assert.Equal(t, testRequest, readRetryingTimeouts(t, c))
		})
	}
}

func TestInfoFromConn(t *testing.T) {
	tests := []struct {
		name string
		conn any
		want ClientInfo
	}{
		// Downstream trackers wrap the Manager's conn.
		{name: "upstream chain", conn: lAdapter.NewTaggedConn(lAdapter.NewTaggedConn(&readConn{info: testInfo}, "a"), "b"), want: testInfo},
		{name: "no info", conn: &readConn{}},
		{name: "no carrier", conn: lAdapter.NewTaggedConn(nil, "a")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := InfoFromConn(tt.conn)
			assert.Equal(t, tt.want != ClientInfo{}, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// stubPacketConn delivers canned datagrams in order, then io.EOF, and records
// the datagrams written back.
type stubPacketConn struct {
	N.PacketConn
	packets []string
	written []string
}

func (c *stubPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	if len(c.packets) == 0 {
		return M.Socksaddr{}, io.EOF
	}
	packet := c.packets[0]
	c.packets = c.packets[1:]
	_, err := b.WriteString(packet)
	return M.Socksaddr{}, err
}

func (c *stubPacketConn) WritePacket(b *buf.Buffer, _ M.Socksaddr) error {
	c.written = append(c.written, string(b.Bytes()))
	return nil
}

// readAllPackets reads datagrams from c until io.EOF.
func readAllPackets(t *testing.T, c *readPacketConn) []string {
	t.Helper()
	var got []string
	for {
		buffer := buf.NewPacket()
		_, err := c.ReadPacket(buffer)
		if err != nil {
			buffer.Release()
			require.ErrorIs(t, err, io.EOF)
			return got
		}
		got = append(got, string(buffer.Bytes()))
		buffer.Release()
	}
}

func TestReadPacketInfo(t *testing.T) {
	tests := []struct {
		name     string
		packet   string
		wantInfo ClientInfo
		wantAcks []string
		wantRest []string
	}{
		{name: "frame", packet: clientInfoPrefix + testInfoJSON, wantInfo: testInfo},
		{name: "legacy frame", packet: legacyClientInfoPrefix + testInfoJSON, wantInfo: testInfo, wantAcks: []string{ackResponse}},
		{name: "empty info", packet: clientInfoPrefix + "{}"},
		{name: "no frame", packet: "datagram", wantRest: []string{"datagram"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubPacketConn{packets: []string{tt.packet}}
			c := &readPacketConn{PacketConn: stub}

			info, err := c.readInfo()
			require.NoError(t, err)
			assert.Equal(t, tt.wantInfo, info)
			assert.Equal(t, tt.wantAcks, stub.written)
			assert.Equal(t, tt.wantRest, readAllPackets(t, c))
		})
	}
}

// On a datagram transport a frame can arrive after the flow's first datagram.
// Forwarding it would leak client info to the destination.
func TestReadPacketDrop(t *testing.T) {
	t.Run("late frames", func(t *testing.T) {
		stub := &stubPacketConn{packets: []string{
			"first",
			clientInfoPrefix + testInfoJSON,
			"second",
			legacyClientInfoPrefix + testInfoJSON,
			"third",
		}}
		c := &readPacketConn{PacketConn: stub}

		info, err := c.readInfo()
		require.NoError(t, err)
		require.Zero(t, info)
		assert.Equal(t, []string{"first", "second", "third"}, readAllPackets(t, c))
	})

	// Packet writers beneath the copy loop prepend their headers into the
	// caller's front headroom.
	t.Run("keeps headroom", func(t *testing.T) {
		stub := &stubPacketConn{packets: []string{clientInfoPrefix + "{}", "datagram"}}
		c := &readPacketConn{PacketConn: stub}

		buffer := buf.NewPacket()
		defer buffer.Release()
		const headroom = 64
		buffer.Resize(headroom, 0)
		_, err := c.ReadPacket(buffer)
		require.NoError(t, err)
		assert.Equal(t, headroom, buffer.Start())
		assert.Equal(t, "datagram", string(buffer.Bytes()))
	})
}
