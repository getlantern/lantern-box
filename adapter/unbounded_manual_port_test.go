package adapter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnboundedManualPortFromContext(t *testing.T) {
	assert.Zero(t, UnboundedManualPortFromContext(context.Background()), "no port by default")
	assert.Zero(t, UnboundedManualPortFromContext(ContextWithUnboundedManualPort(context.Background(), 0)),
		"a zero port registers nothing")
	assert.Equal(t, uint16(40123), UnboundedManualPortFromContext(
		ContextWithUnboundedManualPort(context.Background(), 40123)))
}
