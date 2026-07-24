package group

import (
	"errors"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getlantern/lantern-box/adapter"
)

const (
	defaultLocalCapacityBackoff = time.Second
	// WSAENOBUFS is 10055. Keep the errno portable so the classifier and its
	// wrapping tests run on every build host.
	windowsWSAENOBUFS = syscall.Errno(10055)
)

var globalLocalCapacityGate localCapacityGate

type localCapacityGate struct {
	until atomic.Int64
}

func (g *localCapacityGate) remaining(now time.Time) time.Duration {
	return max(0, time.Duration(g.until.Load()-now.UnixNano()))
}

func (g *localCapacityGate) start(now time.Time, backoff time.Duration) time.Duration {
	until := now.Add(backoff).UnixNano()
	for {
		current := g.until.Load()
		if current >= until {
			until = current
			break
		}
		if g.until.CompareAndSwap(current, until) {
			break
		}
	}
	return max(0, time.Duration(until-now.UnixNano()))
}

func (s *MutableAutoSelect) currentLocalCapacityError() *adapter.LocalCapacityError {
	retryAfter := s.localCapacityGate().remaining(time.Now())
	if retryAfter <= 0 {
		return nil
	}
	return &adapter.LocalCapacityError{RetryAfter: retryAfter}
}

func (s *MutableAutoSelect) localCapacityErrorFor(err error) *adapter.LocalCapacityError {
	if !errors.Is(err, windowsWSAENOBUFS) && !errors.Is(err, adapter.ErrLocalCapacity) {
		return s.currentLocalCapacityError()
	}
	capacityErr := &adapter.LocalCapacityError{
		Err:        err,
		RetryAfter: s.localCapacityGate().start(time.Now(), defaultLocalCapacityBackoff),
	}
	s.emitLocalCapacity(capacityErr)
	return capacityErr
}

func (s *MutableAutoSelect) localCapacityGate() *localCapacityGate {
	if s.capacityGate != nil {
		return s.capacityGate
	}
	return &globalLocalCapacityGate
}

func (s *MutableAutoSelect) emitLocalCapacity(capacityErr *adapter.LocalCapacityError) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return
	}
	select {
	case <-s.localCapacityCh:
	default:
	}
	select {
	case s.localCapacityCh <- capacityErr:
	default:
	}
}
