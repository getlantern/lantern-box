package unbounded

import (
	"fmt"
	"math"
	"strconv"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
)

// tuningKeys maps each recognized key of the outbound's tuning option onto the WebRTCTuning field
// it sets. Durations are in milliseconds and sizes in bytes; every value is a decimal string.
var tuningKeys = map[string]func(*UBClientcore.WebRTCTuning, uint64){
	"ice_max_binding_requests":    func(t *UBClientcore.WebRTCTuning, v uint64) { t.ICEMaxBindingRequests = uint16(v) },
	"stun_gather_timeout_ms":      func(t *UBClientcore.WebRTCTuning, v uint64) { t.STUNGatherTimeout = ms(v) },
	"ice_disconnected_timeout_ms": func(t *UBClientcore.WebRTCTuning, v uint64) { t.ICEDisconnectedTimeout = ms(v) },
	"ice_failed_timeout_ms":       func(t *UBClientcore.WebRTCTuning, v uint64) { t.ICEFailedTimeout = ms(v) },
	"ice_keepalive_interval_ms":   func(t *UBClientcore.WebRTCTuning, v uint64) { t.ICEKeepaliveInterval = ms(v) },
	"srflx_acceptance_min_wait_ms": func(t *UBClientcore.WebRTCTuning, v uint64) {
		t.SrflxAcceptanceMinWait = ms(v)
	},
	"dtls_retransmit_ms": func(t *UBClientcore.WebRTCTuning, v uint64) { t.DTLSRetransmitInterval = ms(v) },
	"sctp_rto_max_ms":    func(t *UBClientcore.WebRTCTuning, v uint64) { t.SCTPRTOMax = ms(v) },
	"sctp_max_receive_buffer_bytes": func(t *UBClientcore.WebRTCTuning, v uint64) {
		t.SCTPMaxReceiveBuffer = uint32(v)
	},
}

// tuningMax bounds each key so a bad value can't make pion wait for days or allocate gigabytes.
// Keys not listed are bounded by maxTuningMs.
var tuningMax = map[string]uint64{
	"ice_max_binding_requests":      math.MaxUint16,
	"sctp_max_receive_buffer_bytes": 64 << 20,
}

const maxTuningMs = 10 * 60 * 1000

func ms(v uint64) time.Duration { return time.Duration(v) * time.Millisecond }

// parseTuning turns the outbound's tuning option into a WebRTCTuning. Keys it doesn't recognize
// are reported in ignored rather than rejected: the option is a map precisely so that a server can
// send knobs newer clients understand without breaking older ones, which is the whole point of
// tuning from config. A recognized key with a bad value is reported too and left at its default.
func parseTuning(opts map[string]string) (tuning UBClientcore.WebRTCTuning, ignored []string) {
	for k, raw := range opts {
		set, ok := tuningKeys[k]
		if !ok {
			ignored = append(ignored, k)
			continue
		}
		limit, ok := tuningMax[k]
		if !ok {
			limit = maxTuningMs
		}
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || v == 0 || v > limit {
			ignored = append(ignored, fmt.Sprintf("%s=%q", k, raw))
			continue
		}
		set(&tuning, v)
	}
	return tuning, ignored
}
