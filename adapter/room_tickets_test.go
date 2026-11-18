package adapter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoomTicketSourceFromContext(t *testing.T) {
	assert.Nil(t, RoomTicketSourceFromContext(context.Background()), "no source by default")
	assert.Nil(t, RoomTicketSourceFromContext(ContextWithRoomTicketSource(context.Background(), nil)),
		"a nil source registers nothing")

	calls := 0
	ctx := ContextWithRoomTicketSource(context.Background(), func() []string {
		calls++
		return []string{"a.b"}
	})
	src := RoomTicketSourceFromContext(ctx)
	require.NotNil(t, src)
	assert.Equal(t, []string{"a.b"}, src())
	assert.Equal(t, []string{"a.b"}, src())
	assert.Equal(t, 2, calls, "the source is called on every read, so its tickets can change")
}
