package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

const probeAPIHost = "api.iantem.io"

// probeServer is a lantern-cloud launch config with the banditprobe outbound
// and the probe route rule pcfg.injectBanditProbe adds.
func probeServer(inbound string) string {
	return `{"log":{"level":"warn"},"inbounds":[` + inbound + `],
	"outbounds":[{"type":"direct","tag":"direct"},
	{"type":"banditprobe","tag":"bandit-probe","callback_url":"https://` + probeAPIHost + `/v1/bandit/callback","stall_timeout":"6s","report_stalled":true}],
	"route":{"rules":[{"domain":["` + probeAPIHost + `"],"port":[80],"action":"route","outbound":"bandit-probe"}]}}`
}

// probeThrough sends a bandit probe through a client box to a server box and
// checks that the responder answered it with the whole body.
func probeThrough(t *testing.T, serverInbound, clientOutbound string) {
	ctx := box.BaseContext()
	serverPort, clientPort := freePort(t), freePort(t)
	srv := fmt.Sprintf(probeServer(serverInbound), serverPort)
	cli := fmt.Sprintf(`{"log":{"level":"warn"},"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":%d}],"outbounds":[`+clientOutbound+`]}`, clientPort, serverPort)

	so, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(srv))
	require.NoError(t, err)
	co, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(cli))
	require.NoError(t, err)
	sb, err := sbox.New(sbox.Options{Context: ctx, Options: so})
	require.NoError(t, err)
	require.NoError(t, sb.Start())
	t.Cleanup(func() { sb.Close() })
	cb, err := sbox.New(sbox.Options{Context: ctx, Options: co})
	require.NoError(t, err)
	require.NoError(t, cb.Start())
	t.Cleanup(func() { cb.Close() })

	client := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return client.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
	}}
	resp, err := hc.Get("http://" + probeAPIHost + "/v1/bandit/callback?token=abc&device=x")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, 64*1024, len(body), "the responder answers with a 64 KiB body")
}

const ssPassword = "d4M6fAj9Q07uK4HePntATUAs1KBceMzq8hGPy1AFxFA="

// The responder used to write its whole response in one Write, which the
// shadowsocks 2022 inbound framed as a single chunk past its 64 KiB - 1 limit
// and panicked on, so no shadowsocks route ever answered a probe.
func TestBanditProbeAnsweredThroughShadowsocks2022(t *testing.T) {
	probeThrough(t,
		`{"type":"shadowsocks","tag":"shadowsocks-in","listen":"127.0.0.1","listen_port":%d,"network":"tcp","method":"2022-blake3-chacha20-poly1305","password":"`+ssPassword+`"}`,
		`{"type":"shadowsocks","tag":"ss-out","server":"127.0.0.1","server_port":%d,"method":"2022-blake3-chacha20-poly1305","password":"`+ssPassword+`"}`)
}

func TestBanditProbeAnsweredThroughVLESS(t *testing.T) {
	probeThrough(t,
		`{"type":"vless","tag":"vless-in","listen":"127.0.0.1","listen_port":%d,"users":[{"uuid":"84fe59e3-179f-4469-8326-a52cf6487d84"}]}`,
		`{"type":"vless","tag":"vless-out","server":"127.0.0.1","server_port":%d,"uuid":"84fe59e3-179f-4469-8326-a52cf6487d84"}`)
}
