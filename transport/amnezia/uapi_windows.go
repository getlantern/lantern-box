package amnezia

import (
	"fmt"
	"net"

	"github.com/sagernet/wireguard-go/ipc"
)

func uapiListen(name string) (net.Listener, error) {
	uapi, err := ipc.UAPIListen(name)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on uapi socket: %v", err)
	}
	return uapi, nil
}
