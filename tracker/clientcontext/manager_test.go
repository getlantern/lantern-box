package clientcontext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replayConn serves a fixed stream and records writes.
type replayConn struct {
	net.Conn
	stream  io.Reader
	written bytes.Buffer
}

func (c *replayConn) Read(p []byte) (int, error)  { return c.stream.Read(p) }
func (c *replayConn) Write(p []byte) (int, error) { return c.written.Write(p) }

func TestReadInfoRestoresPayloadBehindFrame(t *testing.T) {
	frame, err := json.Marshal(ClientInfo{DeviceID: "test-device", Platform: "linux"})
	require.NoError(t, err)
	const payload = "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"

	stream := &replayConn{stream: strings.NewReader(packetPrefix + string(frame) + payload)}
	conn := &readConn{Conn: stream, reader: stream}

	info, err := conn.readInfo()
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "test-device", info.DeviceID)
	assert.Empty(t, stream.written.String(), "the frame must not be acknowledged")

	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Equal(t, payload, string(got), "payload sent behind the frame must still reach the destination")
}

// upstreamConn exposes the wrapped connection through Upstream.
type upstreamConn struct {
	net.Conn
}

func (c upstreamConn) Upstream() any { return c.Conn }

func TestInfoFromConnWalksUpstreamChain(t *testing.T) {
	info := ClientInfo{DeviceID: "dev-9", Platform: "linux"}
	carrier := &readConn{info: &info}
	// Wrap the carrier twice, as downstream trackers would.
	chain := upstreamConn{Conn: upstreamConn{Conn: carrier}}

	got, ok := InfoFromConn(chain)
	require.True(t, ok, "info must resolve through the Upstream chain")
	assert.Equal(t, info, got)
}

func TestInfoFromConnReturnsFalseWithoutCarrier(t *testing.T) {
	// Cover a carrier with no info and a chain with no carrier.
	_, ok := InfoFromConn(&readConn{})
	assert.False(t, ok, "a carrier with no frame reports no info")

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	_, ok = InfoFromConn(upstreamConn{Conn: server})
	assert.False(t, ok, "a chain with no carrier reports no info")
}

// TCP can split the marker across reads. The server must classify the stream
// from accumulated bytes, not a single Read.
func TestReadInfoRecognizesSplitPrefix(t *testing.T) {
	for _, chunk := range []int{1, 5, 10, 11, 32} {
		t.Run(fmt.Sprintf("%d-byte-chunks", chunk), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			payload, err := json.Marshal(ClientInfo{DeviceID: "test-device", Platform: "test"})
			require.NoError(t, err)
			packet := append([]byte(packetPrefix), payload...)

			// net.Pipe is unbuffered, so each Write lands as its own Read.
			go func() {
				for off := 0; off < len(packet); off += chunk {
					end := min(off+chunk, len(packet))
					if _, err := client.Write(packet[off:end]); err != nil {
						return
					}
				}
				io.Copy(io.Discard, client) // Drain any reply.
			}()

			c := &readConn{Conn: server, reader: server, mgr: &Manager{}}

			done := make(chan struct{})
			var info *ClientInfo
			var readErr error
			go func() {
				info, readErr = c.readInfo()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("readInfo blocked")
			}

			require.NoError(t, readErr)
			require.NotNil(t, info, "client info must be recognized when the prefix arrives in %d-byte chunks", chunk)
			assert.Equal(t, "test-device", info.DeviceID)
		})
	}
}

// A peer whose whole first message is shorter than the prefix is not sending
// client info. Those bytes must still reach the destination unaltered, and the
// flow must not be failed.
func TestReadInfoPassesThroughShortNonClientInfo(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		client.Write([]byte("hi"))
		client.Close()
	}()

	c := &readConn{Conn: server, reader: server, mgr: &Manager{}}
	info, err := c.readInfo()
	require.NoError(t, err, "a short non-client-info opening must not fail the flow")
	require.Nil(t, info)

	got, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, "hi", string(got), "the bytes already consumed must be replayed to the destination")
}

// Ordinary traffic longer than the prefix keeps flowing untouched.
func TestReadInfoPassesThroughNonClientInfo(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const payload = "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
	go func() {
		client.Write([]byte(payload))
		client.Close()
	}()

	c := &readConn{Conn: server, reader: server, mgr: &Manager{}}
	info, err := c.readInfo()
	require.NoError(t, err)
	require.Nil(t, info)

	got, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, payload, string(got))
}

// A peer that closes without sending anything leaves nothing to pass through.
// The read error is returned and stored unchanged, so RoutedConnection's
// `err != c.readErr` check treats it as a dead connection rather than
// reporting it as a client-info failure.
func TestReadInfoPropagatesErrorWhenNothingWasRead(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	client.Close()

	c := &readConn{Conn: server, reader: server, mgr: &Manager{}}
	info, err := c.readInfo()

	require.Error(t, err)
	require.Nil(t, info)
	assert.Equal(t, err, c.readErr, "the caller distinguishes a dead conn from bad client info by identity")
	assert.Zero(t, c.n)
}

func TestReadInfoAcknowledgesLegacyClient(t *testing.T) {
	frame, err := json.Marshal(ClientInfo{DeviceID: "old-device", Platform: "linux"})
	require.NoError(t, err)

	stream := &replayConn{stream: strings.NewReader(legacyPacketPrefix + string(frame))}
	conn := &readConn{Conn: stream, reader: stream}

	info, err := conn.readInfo()
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "old-device", info.DeviceID)
	assert.Equal(t, ackResponse, stream.written.String(), "a legacy client must still be acknowledged")
}

// stubServerPacketConn delivers one canned datagram to readPacketConn and records
// the datagrams written back, exercising the server UDP frame path in isolation.
type stubServerPacketConn struct {
	N.PacketConn
	packet  []byte
	read    bool
	written []string
}

func (c *stubServerPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	if c.read {
		return M.Socksaddr{}, io.EOF
	}
	c.read = true
	if _, err := b.Write(c.packet); err != nil {
		return M.Socksaddr{}, err
	}
	return M.Socksaddr{}, nil
}

func (c *stubServerPacketConn) WritePacket(b *buf.Buffer, _ M.Socksaddr) error {
	c.written = append(c.written, string(b.Bytes()))
	return nil
}

// A current client's datagram is decoded and left unacknowledged.
func TestReadPacketInfoCurrentMarkerNotAcknowledged(t *testing.T) {
	payload, err := json.Marshal(ClientInfo{DeviceID: "udp-device", Platform: "test"})
	require.NoError(t, err)
	stub := &stubServerPacketConn{packet: append([]byte(packetPrefix), payload...)}
	c := &readPacketConn{PacketConn: stub, mgr: &Manager{}}

	info, err := c.readInfo()
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "udp-device", info.DeviceID)
	assert.Empty(t, stub.written, "a current client must not be acknowledged")
}

// A legacy client's datagram is decoded and acknowledged with ackResponse.
func TestReadPacketInfoLegacyMarkerAcknowledged(t *testing.T) {
	payload, err := json.Marshal(ClientInfo{DeviceID: "udp-legacy", Platform: "test"})
	require.NoError(t, err)
	stub := &stubServerPacketConn{packet: append([]byte(legacyPacketPrefix), payload...)}
	c := &readPacketConn{PacketConn: stub, mgr: &Manager{}}

	info, err := c.readInfo()
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "udp-legacy", info.DeviceID)
	assert.Equal(t, []string{ackResponse}, stub.written, "a legacy client must be acknowledged")
}

// A datagram that is not a client-info frame is never acknowledged and stays
// readable: readInfo caches it for replay as ordinary traffic.
func TestReadPacketInfoNonFrameReplayed(t *testing.T) {
	const datagram = "not a client info datagram"
	stub := &stubServerPacketConn{packet: []byte(datagram)}
	c := &readPacketConn{PacketConn: stub, mgr: &Manager{}}

	info, err := c.readInfo()
	require.NoError(t, err)
	require.Nil(t, info)
	assert.Empty(t, stub.written, "a non-frame datagram must not be acknowledged")

	buffer := buf.NewPacket()
	defer buffer.Release()
	_, err = c.ReadPacket(buffer)
	require.NoError(t, err)
	assert.Equal(t, datagram, string(buffer.Bytes()), "the datagram must remain readable")
}
