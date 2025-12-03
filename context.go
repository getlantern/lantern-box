package box

import (
	"context"

	box "github.com/sagernet/sing-box"

	"github.com/getlantern/lantern-box/protocol"
)

// BoxContext returns a box context with the registered inbound, outbound, and endpoint protocols.
func BoxContext() context.Context {
	in, out, ep := protocol.GetRegistries()
	return box.Context(context.Background(), in, out, ep)
}
