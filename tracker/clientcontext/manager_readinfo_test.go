package clientcontext

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The server decides whether a flow carries client info by prefix-matching what
// it has read so far. TCP is a stream, so the prefix can arrive split across
// reads -- routine under DPI throttling. Judging one Read's worth would classify
// client info as ordinary traffic: no OK is ever sent, the client blocks on its
// response read, and the prefix bytes are forwarded on to the destination.
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
				io.Copy(io.Discard, client) // drain the OK
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
