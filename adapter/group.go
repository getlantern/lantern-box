package adapter

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

var (
	// ErrGroupClosed is returned by MutableOutboundGroup Add/Remove when the
	// group has already been torn down.
	ErrGroupClosed = errors.New("group is closed")
	// ErrLocalCapacity identifies a host-level resource failure rather than
	// a failure of the selected outbound.
	ErrLocalCapacity = errors.New("local socket capacity exhausted")
)

// LocalCapacityError reports a host-level resource failure and how long
// callers should wait before retrying. Err retains the underlying OS error.
type LocalCapacityError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *LocalCapacityError) Error() string {
	switch {
	case e == nil:
		return ErrLocalCapacity.Error()
	case e.Err == nil && e.RetryAfter <= 0:
		return ErrLocalCapacity.Error()
	case e.Err == nil:
		return fmt.Sprintf("%s; retry after %s", ErrLocalCapacity, e.RetryAfter)
	case e.RetryAfter <= 0:
		return fmt.Sprintf("%s: %v", ErrLocalCapacity, e.Err)
	default:
		return fmt.Sprintf("%s; retry after %s: %v", ErrLocalCapacity, e.RetryAfter, e.Err)
	}
}

func (e *LocalCapacityError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *LocalCapacityError) Is(target error) bool {
	return target == ErrLocalCapacity
}

type MutableOutboundGroup interface {
	adapter.OutboundGroup
	Add(tags ...string) (n int, err error)
	Remove(tags ...string) (n int, err error)
}

// URLOverrideSetter is implemented by outbound groups that support per-outbound URL test overrides.
type URLOverrideSetter interface {
	SetURLOverrides(overrides map[string]string)
}

// OutboundChecker is implemented by outbound groups that support on-demand URL testing.
type OutboundChecker interface {
	CheckOutbounds()
}

// ExhaustionSignaler exposes a channel the host can select on to learn that
// the group's reconnection ladder finished without finding a working
// candidate. At most one value is sent per ladder run; the host decides
// what to do (typically a rate-limited config refetch).
type ExhaustionSignaler interface {
	ExhaustionSignal() <-chan struct{}
}

// LocalCapacitySignaler exposes host-level socket-capacity failures without
// attributing them to a member of the group.
type LocalCapacitySignaler interface {
	LocalCapacitySignal() <-chan *LocalCapacityError
}

// TaggedConn is a net.Conn tagged with the outbound tag used to create it.
type TaggedConn struct {
	net.Conn
	outboundTag string
}

func NewTaggedConn(conn net.Conn, outboundTag string) *TaggedConn {
	return &TaggedConn{
		Conn:        conn,
		outboundTag: outboundTag,
	}
}

func (c *TaggedConn) Tag() string {
	return c.outboundTag
}

// TaggedPacketConn is a net.PacketConn tagged with the outbound tag used to create it.
type TaggedPacketConn struct {
	net.PacketConn
	outboundTag string
}

func NewTaggedPacketConn(conn net.PacketConn, outboundTag string) *TaggedPacketConn {
	return &TaggedPacketConn{
		PacketConn:  conn,
		outboundTag: outboundTag,
	}
}

func (c *TaggedPacketConn) Tag() string {
	return c.outboundTag
}
