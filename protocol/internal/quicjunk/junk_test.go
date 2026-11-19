package quicjunk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewJunkShape(t *testing.T) {
	for range 1000 {
		junk, err := New()
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(junk), 8)
		require.LessOrEqual(t, len(junk), 64)
		require.NotZero(t, junk[0]&0x80, "long-header bit")
	}
}
