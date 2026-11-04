package adapter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnboundedPortMappingFromContext(t *testing.T) {
	port, enabled := UnboundedPortMappingFromContext(context.Background())
	assert.False(t, enabled, "mapping is off by default")
	assert.Zero(t, port)

	port, enabled = UnboundedPortMappingFromContext(ContextWithUnboundedPortMapping(context.Background(), 0))
	assert.True(t, enabled, "a zero port enables automatic mapping")
	assert.Zero(t, port)

	port, enabled = UnboundedPortMappingFromContext(ContextWithUnboundedPortMapping(context.Background(), 40123))
	assert.True(t, enabled)
	assert.Equal(t, uint16(40123), port)
}
