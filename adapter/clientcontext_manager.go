package adapter

import "github.com/sagernet/sing-box/adapter"

type ClientContextManager interface {
	adapter.ConnectionTracker
	AppendTracker(adapter.ConnectionTracker)
}
