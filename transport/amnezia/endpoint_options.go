// Package amnezia implements the amnezia transport endpoint
package amnezia

import (
	"github.com/getlantern/lantern-box/option"

	"github.com/sagernet/sing-box/transport/wireguard"
)

type EndpointOptions struct {
	wireguard.EndpointOptions
	option.AmneziaOptions
}
