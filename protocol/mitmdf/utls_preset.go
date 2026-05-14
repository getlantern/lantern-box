package mitmdf

import (
	"fmt"

	utls "github.com/refraction-networking/utls"
)

// presetFromString maps the user-facing fingerprint string to a uTLS
// ClientHelloID preset. The empty string defaults to chrome.
func presetFromString(s string) (utls.ClientHelloID, error) {
	switch s {
	case "", "chrome":
		return utls.HelloChrome_Auto, nil
	case "firefox":
		return utls.HelloFirefox_Auto, nil
	case "safari":
		return utls.HelloSafari_Auto, nil
	case "random":
		return utls.HelloRandomized, nil
	}
	return utls.ClientHelloID{}, fmt.Errorf("unknown fingerprint %q (want chrome|firefox|safari|random)", s)
}

// applyPresetWithALPN materializes the ClientHello spec for helloID,
// overwrites the ALPN extension's protocol list with alpn, and applies it
// to uConn. The caller MUST have constructed uConn with utls.HelloCustom
// so applyPresetByID at handshake time is a no-op rather than re-applying
// the preset's hard-coded ALPN.
//
// This is the load-bearing piece that makes ALPN inheritance from the
// user's negotiated protocol actually take effect on the wire. Named uTLS
// presets (HelloChrome_Auto, etc.) embed an ALPN extension with
// [h2, http/1.1] that overrides Config.NextProtos at handshake time. The
// only escape is to route through HelloCustom + ApplyPreset.
func applyPresetWithALPN(uConn *utls.UConn, helloID utls.ClientHelloID, alpn []string) error {
	spec, err := utls.UTLSIdToSpec(helloID)
	if err != nil {
		return fmt.Errorf("uTLSIdToSpec: %w", err)
	}
	found := false
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			a.AlpnProtocols = append(a.AlpnProtocols[:0], alpn...)
			found = true
		}
	}
	if !found {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{
			AlpnProtocols: append([]string(nil), alpn...),
		})
	}
	return uConn.ApplyPreset(&spec)
}
