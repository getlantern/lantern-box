//go:build !with_quic

package unbounded

import (
	"context"
	"net"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
)

// exitPaths needs sing-quic: without with_quic the outbound takes no direct exits.
type exitPaths struct{}

func newExitPaths(context.Context, logger.ContextLogger, string) *exitPaths { return nil }

func (*exitPaths) onExitPath(ep UBClientcore.ExitPath) { _ = ep.Conn.Close() }
func (*exitPaths) takeExit() bool                      { return false }
func (*exitPaths) dial(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, errNoExitPath
}
func (*exitPaths) listenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errNoExitPath
}
func (*exitPaths) close() {}
