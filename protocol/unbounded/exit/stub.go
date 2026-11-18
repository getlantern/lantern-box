//go:build !with_quic

package exit

import (
	"context"
	"errors"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/logger"
)

// ErrUnavailable is what New returns in a build without with_quic.
var ErrUnavailable = errors.New("unbounded exit: built without with_quic")

// InboundTag is the inbound every direct exit stream and flow is routed as.
const InboundTag = "unbounded-exit"

// Exit is unavailable without with_quic: New always fails.
type Exit struct{}

func New(context.Context, logger.ContextLogger) (*Exit, error) { return nil, ErrUnavailable }
func (*Exit) SetEnabled(bool)                                  {}
func (*Exit) SetRouter(adapter.ConnectionRouterEx, time.Time)  {}
func (*Exit) Options() *UBClientcore.ExitOptions               { return nil }
func (*Exit) Close()                                           {}
