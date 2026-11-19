package quicjunk

import (
	"bytes"
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

type datagrams [][]byte

func (d *datagrams) Write(b []byte) (int, error) {
	*d = append(*d, bytes.Clone(b))
	return len(b), nil
}

func TestWriteSendsOneJunkDatagram(t *testing.T) {
	var d datagrams
	require.NoError(t, Write(&d))
	require.Len(t, d, 1)
	require.GreaterOrEqual(t, len(d[0]), 8)
	require.LessOrEqual(t, len(d[0]), 64)
	require.NotZero(t, d[0][0]&0x80, "long-header bit")
}
