package option

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestUnboundedOutboundOptionsMarshalJSON(t *testing.T) {
	opts := UnboundedOutboundOptions{
		DiscoverySrv:      "http://127.0.0.1:9000",
		DiscoveryEndpoint: "/v1/signal",
		STUNBatch: func(size uint32) ([]string, error) {
			return []string{"stun:example.com:3478"}, nil
		},
		HTTPClient: &http.Client{},
	}

	data, err := json.Marshal(opts)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var roundtrip UnboundedOutboundOptions
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if roundtrip.DiscoverySrv != opts.DiscoverySrv {
		t.Errorf("DiscoverySrv = %q, want %q", roundtrip.DiscoverySrv, opts.DiscoverySrv)
	}
	if roundtrip.DiscoveryEndpoint != opts.DiscoveryEndpoint {
		t.Errorf("DiscoveryEndpoint = %q, want %q", roundtrip.DiscoveryEndpoint, opts.DiscoveryEndpoint)
	}
	if roundtrip.STUNBatch != nil {
		t.Error("STUNBatch should be nil after round-trip (json:\"-\")")
	}
	if roundtrip.HTTPClient != nil {
		t.Error("HTTPClient should be nil after round-trip (json:\"-\")")
	}
}
