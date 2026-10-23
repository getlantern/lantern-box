package adapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactHelperTags(t *testing.T) {
	for in, want := range map[string]string{
		"helper-0123456789ab":                           "helper",
		"a helper-0123456789ab, b helper-abcdefabcdef.": "a helper, b helper.",
		"outbound/unbounded[helper-0123456789ab]":       "outbound/unbounded[helper]",
		"no tag here":          "no tag here",
		"":                     "",
		"unbounded-out-pool1":  "unbounded-out-pool1",
		"helper-text":          "helper-text",
		"helper-0123456789abc": "helper-0123456789abc",
		"xhelper-0123456789ab": "xhelper-0123456789ab",
		"helper-0123456789AB":  "helper-0123456789AB",
	} {
		assert.Equal(t, want, RedactHelperTags(in), "input %q", in)
	}
	assert.Equal(t, "helper-", HelperTagPrefix)
}

func BenchmarkRedactHelperTagsNoMatch(b *testing.B) {
	s := "[1] outbound connection to example.com:443 using outbound/shadowsocks[ss-out-pool1]"
	for i := 0; i < b.N; i++ {
		_ = RedactHelperTags(s)
	}
}
