package unbounded

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	UBCommon "github.com/getlantern/broflake/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
)

// subscribe sends a request shaped like broflake's consumer subscribe, with
// the room tickets header, through the outbound's signaling client.
func subscribe(t *testing.T, o *Outbound, target string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	req.Header.Set(UBCommon.RoomTicketsHeader, "a.b")
	return o.rtcOpt.HTTPClient.Do(req)
}

func TestSignalingClient_RefusesCrossOriginRedirects(t *testing.T) {
	var leaked atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(UBCommon.RoomTicketsHeader) != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(other.Close)
	freddie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(freddie.Close)

	ctx := lbAdapter.ContextWithRoomTicketSource(context.Background(), func() []string { return []string{"a.b"} })
	for _, tag := range []string{"helper-0123456789ab", "unbounded-out-pool1"} {
		o := newTestOutbound(t, ctx, tag)
		resp, err := subscribe(t, o, freddie.URL+"/v1/signal")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err, "tag %q: a redirect to another origin must fail", tag)
	}
	assert.Zero(t, leaked.Load(), "the redirect target received the room tickets header")
}

func TestSignalingClient_FollowsSameOriginRedirects(t *testing.T) {
	var got atomic.Value
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, srv.URL+"/new", http.StatusTemporaryRedirect)
			return
		}
		got.Store(r.Header.Get(UBCommon.RoomTicketsHeader))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx := lbAdapter.ContextWithRoomTicketSource(context.Background(), func() []string { return []string{"a.b"} })
	resp, err := subscribe(t, newTestOutbound(t, ctx, "helper-0123456789ab"), srv.URL+"/old")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "a.b", got.Load(), "a same-origin redirect is followed with its headers")
}

func TestSameOrigin(t *testing.T) {
	u := func(s string) *url.URL {
		v, err := url.Parse(s)
		require.NoError(t, err)
		return v
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://f.example/v1", "https://f.example/v2", true},
		{"https://f.example/v1", "https://F.EXAMPLE:443/v2", true},
		{"http://f.example:80/", "http://f.example/", true},
		{"https://f.example/", "http://f.example/", false},
		{"https://f.example/", "https://f.example:8443/", false},
		{"https://f.example/", "https://evil.example/", false},
		{"http://127.0.0.1:1000/", "http://127.0.0.1:1001/", false},
	} {
		assert.Equal(t, c.same, sameOrigin(u(c.a), u(c.b)), "%s vs %s", c.a, c.b)
	}
}

func TestCheckSignalingRedirect_HopLimit(t *testing.T) {
	req := func(s string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, s, nil)
		require.NoError(t, err)
		return r
	}
	via := make([]*http.Request, 0, 10)
	for range 9 {
		via = append(via, req("https://f.example/a"))
	}
	assert.NoError(t, checkSignalingRedirect(req("https://f.example/b"), via))
	via = append(via, req("https://f.example/a"))
	assert.Error(t, checkSignalingRedirect(req("https://f.example/b"), via))
}
