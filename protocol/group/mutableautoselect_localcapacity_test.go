package group

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
)

func TestMutableAutoSelect_LocalCapacityPreservesSelectionHistory(t *testing.T) {
	syscallErr := &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10055)}
	netOpErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscallErr}
	tests := []struct {
		name string
		err  error
	}{
		{name: "errno", err: syscall.Errno(10055)},
		{name: "syscall error", err: syscallErr},
		{name: "net op and syscall errors", err: netOpErr},
		{name: "additional wrapping", err: fmt.Errorf("proxy dial: %w", netOpErr)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, obs := newTestMUR(t, "a", "b")
			s.defaultURL = "http://probe.test/"
			s.recordProbeOutcome("a", true, 10)
			s.recordProbeOutcome("b", true, 20)
			before := s.history.All()
			obs["a"].On("DialContext").Return(nil, tt.err).Once()

			conn, err := s.DialContext(context.Background(), "tcp", M.Socksaddr{})

			require.Nil(t, conn)
			var capacityErr *adapter.LocalCapacityError
			require.ErrorAs(t, err, &capacityErr)
			assert.ErrorIs(t, err, adapter.ErrLocalCapacity)
			assert.ErrorIs(t, err, syscall.Errno(10055))
			assert.Positive(t, capacityErr.RetryAfter)
			assert.Equal(t, before, s.history.All(), "local capacity must not change member history")

			select {
			case signal := <-s.LocalCapacitySignal():
				assert.Same(t, capacityErr, signal)
			default:
				require.FailNow(t, "local capacity signal was not emitted")
			}

			assertLadderNotRun(t, s)
			obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
			obs["b"].AssertNumberOfCalls(t, "DialContext", 0)
			assert.Equal(t, before, s.history.All(), "local capacity must not change member history asynchronously")
		})
	}
}

func TestMutableAutoSelect_LocalCapacityBackoffIsGlobal(t *testing.T) {
	first, firstOutbounds := newTestMUR(t, "a")
	second, secondOutbounds := newTestMUR(t, "b")
	first.capacityGate = &localCapacityGate{}
	second.capacityGate = first.capacityGate
	first.recordProbeOutcome("a", true, 10)
	second.recordProbeOutcome("b", true, 10)
	beforeFirst := first.history.All()
	beforeSecond := second.history.All()
	firstOutbounds["a"].On("DialContext").Return(nil, syscall.Errno(10055)).Once()

	_, firstErr := first.DialContext(context.Background(), "tcp", M.Socksaddr{})
	require.ErrorIs(t, firstErr, adapter.ErrLocalCapacity)

	_, secondErr := second.DialContext(context.Background(), "tcp", M.Socksaddr{})
	var capacityErr *adapter.LocalCapacityError
	require.ErrorAs(t, secondErr, &capacityErr)
	assert.ErrorIs(t, secondErr, adapter.ErrLocalCapacity)
	assert.Nil(t, capacityErr.Err, "backoff rejection should not invent an outbound failure")
	assert.Positive(t, capacityErr.RetryAfter)
	assert.Equal(t, beforeFirst, first.history.All())
	assert.Equal(t, beforeSecond, second.history.All())
	secondOutbounds["b"].AssertNumberOfCalls(t, "DialContext", 0)
}

func TestMutableAutoSelect_PropagatesNestedLocalCapacity(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b")
	s.defaultURL = "http://probe.test/"
	s.recordProbeOutcome("a", true, 10)
	s.recordProbeOutcome("b", true, 20)
	before := s.history.All()
	obs["a"].On("DialContext").Return(nil, &adapter.LocalCapacityError{
		RetryAfter: time.Second,
	}).Once()

	_, err := s.DialContext(context.Background(), "tcp", M.Socksaddr{})

	require.ErrorIs(t, err, adapter.ErrLocalCapacity)
	assert.Equal(t, before, s.history.All())
	assertLadderNotRun(t, s)
	obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
	obs["b"].AssertNumberOfCalls(t, "DialContext", 0)
}

func TestMutableAutoSelect_AlternateLocalCapacityIsNotPenalized(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b")
	s.defaultURL = "http://probe.test/"
	s.recordProbeOutcome("a", true, 10)
	s.recordProbeOutcome("b", true, 20)
	beforeB := s.history.Load("b")
	obs["a"].On("DialContext").Return(nil, errors.New("proxy refused connection")).Once()
	obs["b"].On("DialContext").Return(nil, &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "bind", Err: syscall.Errno(10055)},
	}).Once()

	conn, err := s.DialContext(context.Background(), "tcp", M.Socksaddr{})

	require.Nil(t, conn)
	require.ErrorIs(t, err, adapter.ErrLocalCapacity)
	assert.Len(t, s.history.Load("a").UserFailures, 1, "the actual proxy failure remains attributable")
	assert.Equal(t, beforeB, s.history.Load("b"), "the alternate must not be penalized for host capacity")
	assertLadderNotRun(t, s)
	obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
	obs["b"].AssertNumberOfCalls(t, "DialContext", 1)
}

func TestMutableAutoSelect_ListenPacketHonorsLocalCapacityBackoff(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b")
	s.defaultURL = "http://probe.test/"
	s.recordProbeOutcome("a", true, 10)
	s.recordProbeOutcome("b", true, 20)
	before := s.history.All()
	obs["a"].On("ListenPacket").Return(nil, &os.SyscallError{
		Syscall: "bind",
		Err:     syscall.Errno(10055),
	}).Once()

	conn, err := s.ListenPacket(context.Background(), M.Socksaddr{})

	require.Nil(t, conn)
	require.ErrorIs(t, err, adapter.ErrLocalCapacity)
	assert.Equal(t, before, s.history.All())
	assertLadderNotRun(t, s)
	obs["a"].AssertNumberOfCalls(t, "ListenPacket", 1)
	obs["b"].AssertNumberOfCalls(t, "ListenPacket", 0)
}

func assertLadderNotRun(t *testing.T, s *MutableAutoSelect) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	assert.False(t, s.laddering.Load())
	assert.Zero(t, s.lastLadderAt.Load())
}
