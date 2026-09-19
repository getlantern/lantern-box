package group

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A member that answered carried the request, so the reply going short does not
// demote it — the probe URL's own hiccup is not evidence about the member.
func TestProbeMember_ATruncatedBodyStillReachesTheMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(strings.Repeat("x", 16)))
		// Hand the short body to the client before taking the connection, so
		// the probe reads a truncated body rather than nothing at all.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the test server does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	_, obs := newTestMUR(t, "live")
	obs["live"].dial = func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}

	res := probeMember(context.Background(), obs["live"], srv.URL, protocolBehavior{probeTimeout: time.Second})

	assert.True(t, res.success)
	assert.Positive(t, res.delayMs)
}

func TestProbeMember_EveryOtherFailureDemotes(t *testing.T) {
	_, obs := newTestMUR(t, "dead")
	obs["dead"].dial = func(context.Context) (net.Conn, error) {
		return nil, errors.New("dial denied")
	}

	res := probeMember(context.Background(), obs["dead"], "http://probe.test/", protocolBehavior{probeTimeout: time.Second})

	assert.False(t, res.success)
	assert.Zero(t, res.delayMs)
}

func TestProbeMember_AnExcludedProtocolIsNeverDialed(t *testing.T) {
	_, obs := newTestMUR(t, "excluded")
	obs["excluded"].dial = func(context.Context) (net.Conn, error) {
		t.Fatal("an excluded member must not be dialed")
		return nil, nil
	}

	res := probeMember(context.Background(), obs["excluded"], "http://probe.test/",
		protocolBehavior{probeTimeout: time.Second, excludeFromPool: true})

	assert.False(t, res.success)
}
