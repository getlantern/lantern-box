//go:build !with_wireguard

package amnezia

import (
	"context"
	"errors"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// register a constructor that always errors to follow sing-box's convention
func RegisterEndpoint(registry *endpoint.Registry) {
	endpoint.Register[option.AmneziaEndpointOptions](registry, constant.TypeAmnezia,
		func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.AmneziaEndpointOptions) (adapter.Endpoint, error) {
			return nil, errors.New(`Amnezia is not included in this build, rebuild with -tags with_wireguard`)
		},
	)
}
