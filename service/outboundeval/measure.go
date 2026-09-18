package outboundeval

import (
	"context"
	"net/http"
	"time"

	A "github.com/sagernet/sing-box/adapter"

	"github.com/getlantern/lantern-box/internal/probe"
)

// measureAttempt fetches target once through out and records what happened. It
// never retries: a retry would hide the failure the measurement exists to
// observe.
//
// A response outside 2xx is reported unreachable with failureHTTPStatus, so a
// block page counts against the arm that served it.
func measureAttempt(
	ctx context.Context,
	out A.Outbound,
	target string,
	timeout time.Duration,
	maxBytes int64,
) Attempt {
	result, err := probe.Measure(ctx, out, target, timeout, maxBytes)
	attempt := Attempt{
		HTTPStatus:               result.HTTPStatus,
		TimeToHeadersMS:          result.TimeToHeaders.Milliseconds(),
		ElapsedMS:                result.Elapsed.Milliseconds(),
		BytesRead:                result.BytesRead,
		ThroughputBytesPerSecond: result.ThroughputBytesPerSecond,
	}
	switch {
	case err != nil:
		attempt.FailureCode = classifyFailure(err)
	case result.HTTPStatus < http.StatusOK || result.HTTPStatus >= http.StatusMultipleChoices:
		attempt.FailureCode = failureHTTPStatus
	default:
		attempt.Reachable = true
	}
	return attempt
}

// failedAttempts fills one arm of a window that was never measured.
func failedAttempts(count int, code string) []Attempt {
	attempts := make([]Attempt, count)
	for i := range attempts {
		attempts[i] = Attempt{FailureCode: code}
	}
	return attempts
}
