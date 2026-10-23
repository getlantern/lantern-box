package unbounded

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

func newTestOutbound(t *testing.T, ctx context.Context, tag string) *Outbound {
	t.Helper()
	out, err := NewOutbound(ctx, nil, nil, tag, option.UnboundedOutboundOptions{})
	require.NoError(t, err)
	o, ok := out.(*Outbound)
	require.True(t, ok)
	return o
}

func TestNewOutbound_RoomTicketSourceOnlyForHelperTags(t *testing.T) {
	calls := 0
	ctx := lbAdapter.ContextWithRoomTicketSource(context.Background(), func() []string {
		calls++
		return []string{"a.b"}
	})

	helper := newTestOutbound(t, ctx, "helper-0123456789ab")
	require.NotNil(t, helper.rtcOpt.RoomTicketSource, "a helper- outbound joins its pairings' rooms")
	assert.Equal(t, []string{"a.b"}, helper.rtcOpt.CurrentRoomTickets())
	assert.Equal(t, []string{"a.b"}, helper.rtcOpt.CurrentRoomTickets())
	assert.Equal(t, 2, calls, "broflake reads the source on every subscribe")

	for _, tag := range []string{"unbounded-out-pool1", "", "helper", "xhelper-0123456789ab"} {
		pool := newTestOutbound(t, ctx, tag)
		assert.Nil(t, pool.rtcOpt.RoomTicketSource, "tag %q must not present the inviter's tickets", tag)
		assert.Empty(t, pool.rtcOpt.CurrentRoomTickets(), "tag %q", tag)
	}
	assert.Equal(t, 2, calls, "non-helper outbounds never read the source")
}

func TestNewOutbound_HelperTagWithoutSource(t *testing.T) {
	o := newTestOutbound(t, context.Background(), "helper-0123456789ab")
	assert.Nil(t, o.rtcOpt.RoomTicketSource)
	assert.Empty(t, o.rtcOpt.CurrentRoomTickets())
}

func TestLogTag_RedactsHelperTags(t *testing.T) {
	assert.Equal(t, "helper", logTag("helper-0123456789ab"))
	assert.NotContains(t, logTag("helper-0123456789ab"), "0123456789ab")
	assert.Equal(t, "helper", logTag("helper-"))
	for _, tag := range []string{"unbounded-out-pool1", "", "helper", "xhelper-0123456789ab"} {
		assert.Equal(t, tag, logTag(tag), "non-helper tag %q is logged unchanged", tag)
	}
}
