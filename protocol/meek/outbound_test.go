package meek

import (
	"context"
	"strings"
	"testing"

	"github.com/getlantern/lantern-box/option"
)

// NewOutbound must reject configs that would silently weaken the
// transport: a non-https URL (which would bypass the fronted TLS dialer)
// and a front with no cert identity (which would skip hostname
// verification). Both checks run before the dialer is built, so a nil
// router/logger is fine for these error paths.
func TestNewOutbound_RejectsUnsafeConfig(t *testing.T) {
	base := func() option.MeekOutboundOptions {
		return option.MeekOutboundOptions{
			URL:    "https://meek.example/",
			Fronts: []option.FrontSpec{{IPAddress: "1.2.3.4", VerifyHostname: "a248.e.akamai.net"}},
		}
	}

	t.Run("http scheme rejected", func(t *testing.T) {
		opts := base()
		opts.URL = "http://meek.example/"
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("err = %v; want a scheme-must-be-https error", err)
		}
	})

	t.Run("front without verify_hostname or sni rejected", func(t *testing.T) {
		opts := base()
		opts.Fronts = []option.FrontSpec{{IPAddress: "1.2.3.4"}} // both SNI and VerifyHostname empty
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err == nil || !strings.Contains(err.Error(), "verify_hostname or sni") {
			t.Errorf("err = %v; want a cert-identity-required error", err)
		}
	})

	t.Run("front with sni only is accepted past validation", func(t *testing.T) {
		opts := base()
		opts.Fronts = []option.FrontSpec{{IPAddress: "1.2.3.4", SNI: "cover.example"}}
		// Validation passes; any error must come from later dialer setup,
		// not the front/scheme guards.
		_, err := NewOutbound(context.Background(), nil, nil, "meek", opts)
		if err != nil && (strings.Contains(err.Error(), "https") || strings.Contains(err.Error(), "verify_hostname or sni")) {
			t.Errorf("sni-only front should pass the identity guard, got %v", err)
		}
	})
}
