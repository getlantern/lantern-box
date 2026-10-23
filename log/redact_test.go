package log

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelperTagsRedacted(t *testing.T) {
	var buf bytes.Buffer
	f := NewFactory(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sub, _, err := f.Subscribe()
	require.NoError(t, err)
	defer f.UnSubscribe(sub)

	l := f.NewLogger("outbound/unbounded[helper-0123456789ab]")
	l.InfoContext(context.Background(),
		"[1] outbound connection to example.com:443 using outbound/unbounded[helper-0123456789ab]")
	out := buf.String()
	assert.NotContains(t, out, "0123456789ab", "no helper tag hex in the slog output")
	assert.Contains(t, out, `tag=outbound/unbounded[helper]`)
	assert.Contains(t, out, "using outbound/unbounded[helper]")
	select {
	case entry := <-sub:
		assert.NotContains(t, entry.Message, "0123456789ab", "no helper tag hex for log subscribers")
	case <-time.After(5 * time.Second):
		t.Fatal("no log entry reached the subscriber")
	}

	buf.Reset()
	f.NewLogger("outbound/unbounded[unbounded-out-pool1]").InfoContext(context.Background(),
		"using outbound/unbounded[unbounded-out-pool1] helper-text helper-0123456789abcd xhelper-0123456789ab helper-0123456789AB")
	out = buf.String()
	assert.Contains(t, out, "tag=outbound/unbounded[unbounded-out-pool1]")
	assert.Contains(t, out, "using outbound/unbounded[unbounded-out-pool1] helper-text helper-0123456789abcd xhelper-0123456789ab helper-0123456789AB",
		"pool tags and anything not exactly a word-bounded helper-<12 lowercase hex> are unchanged")
}
