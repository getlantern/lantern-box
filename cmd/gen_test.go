package main

import (
	"fmt"
	"testing"

	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

func TestGenOpts(t *testing.T) {
	options, err := genOpts(config{
		Type:       constant.TypeHysteria2,
		Tag:        "http-in",
		ListenPort: 8080,
		TLS: &tlsConfig{
			Insecure:   true,
			ServerName: "example.com",
			CertPath:   "/path/to/cert.pem",
			KeyPath:    "/path/to/key.pem",
		},
	})
	require.NoError(t, err)

	ctx := box.BaseContext()
	buf, err := json.MarshalContext(ctx, options)
	require.NoError(t, err)
	fmt.Println(string(buf))

	require.NoError(t, checkValid(ctx, options))
}
