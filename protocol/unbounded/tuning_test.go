package unbounded

import (
	"slices"
	"testing"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
)

func TestParseTuning(t *testing.T) {
	got, ignored := parseTuning(map[string]string{
		"ice_max_binding_requests":      "12",
		"stun_gather_timeout_ms":        "2000",
		"ice_disconnected_timeout_ms":   "3000",
		"ice_failed_timeout_ms":         "10000",
		"ice_keepalive_interval_ms":     "1000",
		"srflx_acceptance_min_wait_ms":  "1",
		"dtls_retransmit_ms":            "200",
		"sctp_rto_max_ms":               "3000",
		"sctp_max_receive_buffer_bytes": "4194304",
	})
	want := UBClientcore.WebRTCTuning{
		ICEMaxBindingRequests:  12,
		STUNGatherTimeout:      2 * time.Second,
		ICEDisconnectedTimeout: 3 * time.Second,
		ICEFailedTimeout:       10 * time.Second,
		ICEKeepaliveInterval:   time.Second,
		SrflxAcceptanceMinWait: time.Millisecond,
		DTLSRetransmitInterval: 200 * time.Millisecond,
		SCTPRTOMax:             3 * time.Second,
		SCTPMaxReceiveBuffer:   4 << 20,
	}
	if got != want || len(ignored) != 0 {
		t.Fatalf("got %+v ignored %v, want %+v", got, ignored, want)
	}
}

func TestParseTuningEveryKeyIsTested(t *testing.T) {
	// TestParseTuning sets every key; this keeps a new key from going untested.
	if len(tuningKeys) != 9 {
		t.Fatalf("tuningKeys has %d keys; add the new ones to TestParseTuning", len(tuningKeys))
	}
}

// Unknown keys are how a server sends knobs newer clients understand, so they must be ignored,
// not rejected, and bad values must leave the default in place.
func TestParseTuningIgnoresUnknownAndInvalid(t *testing.T) {
	got, ignored := parseTuning(map[string]string{
		"dtls_retransmit_ms":            "200",
		"from_a_newer_server":           "5",
		"sctp_rto_max_ms":               "abc",
		"stun_gather_timeout_ms":        "0",
		"ice_failed_timeout_ms":         "-5",
		"ice_max_binding_requests":      "70000",
		"sctp_max_receive_buffer_bytes": "1073741824",
		"ice_keepalive_interval_ms":     "999999999",
	})
	if got != (UBClientcore.WebRTCTuning{DTLSRetransmitInterval: 200 * time.Millisecond}) {
		t.Fatalf("only the valid key should apply, got %+v", got)
	}
	if len(ignored) != 7 || !slices.Contains(ignored, "from_a_newer_server") {
		t.Fatalf("ignored %v", ignored)
	}
}

func TestParseTuningEmpty(t *testing.T) {
	if got, ignored := parseTuning(nil); got != (UBClientcore.WebRTCTuning{}) || ignored != nil {
		t.Fatalf("got %+v ignored %v", got, ignored)
	}
}
