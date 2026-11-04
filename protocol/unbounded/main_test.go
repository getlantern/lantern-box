package unbounded

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
)

// TestMain keeps every test in this package off the developer's router and the
// internet, even one that enables port mapping and forgets to swap the pool.
func TestMain(m *testing.M) {
	errReal := errors.New("tests must not reach a real gateway or STUN server")
	sharedMappedPorts.gateway = func(context.Context) (portMapper, error) { return nil, errReal }
	sharedMappedPorts.stunIP = func(context.Context, net.PacketConn, []string) (string, error) { return "", errReal }
	os.Exit(m.Run())
}
