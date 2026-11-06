package samizdatreverse

import (
	"errors"
	"fmt"
	"net"
)

// addrFree strips the addresses a *net.OpError carries, keeping its
// operation and cause, so the error can be logged or returned without
// naming a peer.
func addrFree(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return fmt.Errorf("%s: %w", opErr.Op, opErr.Err)
	}
	return err
}
