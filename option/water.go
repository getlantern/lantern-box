package option

import "github.com/sagernet/sing-box/option"

// WATERInboundOptions specifies the configuration/options for starting a WATER
// listener
type WATERInboundOptions struct {
	option.ListenOptions
	// Transport works as a identifier for the WASM logs
	Transport string `json:"transport"`
	// Hashsum is used for validating if the downloaded WASM
	// hasn't been tampered. Expects a sha256 sum.
	Hashsum string `json:"hashsum"`
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
	WATEROutboundSeedOptions
	WATERDownloadOptions
	// Transport works as a identifier for the WASM logs
	Transport string `json:"transport"`
	// Dir specifies which directory we should use for storing WATER related
	// files
	Dir string `json:"water_dir"`
	// Config is a optional config that will be sent to the WASM file.
	Config map[string]any `json:"config,omitempty"`
	// SkipHandshake is used when the WATER module deals with the handshake
	// instead of the sing-box WATER transport
	SkipHandshake bool `json:"skip_handshake,omitempty"`
	// Optional configuration for supporting UDP over TCP
	UDPOverTCP *option.UDPOverTCPOptions `json:"udp_over_tcp,omitempty"`
}

type WATERDownloadOptions struct {
	// Hashsum is used for validating if the downloaded WASM
	// hasn't been tampered. Expects a sha256 sum.
	Hashsum string `json:"hashsum"`
	// WASMAvailableAt must provide a list of URLs where the WASM file
	// can be downloaded
	WASMAvailableAt []string `json:"wasm_available_at"`
	// DownloadTimeout specifies how much time the downloader should wait
	// until it cancel and try to fetch from another URL
	DownloadTimeout string `json:"download_timeout"`
	// DownloadDetour specifies which outbound tag to use when downloading the WASM file
	DownloadDetour string `json:"download_detour,omitempty"`
}

// WATEROutboundSeedOptions specifies the seed configuration options
type WATEROutboundSeedOptions struct {
	// SeedEnabled enables seeding the used transport
	SeedEnabled bool `json:"seed_enabled,omitempty"`
	// SeedDetour specifies which outbound tag to use when seeding the WASM file
	SeedDetour string `json:"seed_detour,omitempty"`
	// AnnounceList specifies which trackers should be used to announce the file
	AnnounceList [][]string `json:"announce_list,omitempty"`
}
