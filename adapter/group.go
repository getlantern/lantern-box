package adapter

import (
	"github.com/sagernet/sing-box/adapter"
)

type MutableOutboundGroup interface {
	adapter.OutboundGroup
	Add(tags ...string) (n int, err error)
	Remove(tags ...string) (n int, err error)
}
