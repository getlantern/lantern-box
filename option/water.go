package option

import "github.com/sagernet/sing-box/option"

// WATERInboundOptions specifies the configuration/options for starting a WATER
// listener
type WATERInboundOptions struct {
	option.ListenOptions
	// Transport works as a identifier for the WASM logs
	Transport string `json:"transport"`
	// Hashsum is used for validating if the downloaded WASM
	// hasn't been tempered. Expects a sha256 sum.
	Hashsum string `json:hashsum`
	// WASMAvailableAt must provide a list of URLs where the WASM file
	// can be downloaded
	WASMAvailableAt []string `json:"wasm_available_at"`
	// Config is a optional config that will be sent to the WASM file.
	Config map[string]any `json:"config,omitempty"`
}

// WATEROutboundOptions specifies the configuration/options for starting a WATER
// dialer
type WATEROutboundOptions struct {
	option.ServerOptions
	option.DialerOptions
	// Transport works as a identifier for the WASM logs
	Transport string `json:"transport"`
	// Hashsum is used for validating if the downloaded WASM
	// hasn't been tempered. Expects a sha256 sum.
	Hashsum string `json:hashsum`
	// WASMAvailableAt must provide a list of URLs where the WASM file
	// can be downloaded
	WASMAvailableAt []string `json:"wasm_available_at"`
	// DownloadTimeout specifies how much time the downloader should wait
	// until it cancel and try to fetch from another URL
	DownloadTimeout string `json:"download_timeout"`
	// Dir specifies which directory we should use for storing WATER related
	// files
	Dir string `json:"water_dir"`
	// Config is a optional config that will be sent to the WASM file.
	Config map[string]any `json:"config,omitempty"`
	// SkipHandshake is used when the WATER module deals with the handshake
	// instead of the sing-box WATER transport
	SkipHandshake bool `json:"skip_handshake,omitempty"`
}
