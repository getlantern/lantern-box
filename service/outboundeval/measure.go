package outboundeval

import (
	"context"
	"time"

	A "github.com/sagernet/sing-box/adapter"

	"github.com/getlantern/lantern-box/internal/probe"
)

// measureOnce fetches target once through out, under the limits this service
// was configured with.
func (s *Service) measureOnce(ctx context.Context, out A.Outbound, target string) Attempt {
	return measureAttempt(ctx, out, target,
		time.Duration(s.options.RequestTimeout), s.options.MaxResponseBytes)
}

// measureAttempt fetches target once through out and records what happened. It
// never retries: a retry would hide the failure the measurement exists to
// observe.
//
// A response outside 2xx is reported unreachable with failureHTTPStatus even
// when its body then fails, so a block page counts against the arm that served
// it.
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
		TimeToHeadersMS:          float64(result.TimeToHeaders) / float64(time.Millisecond),
		ElapsedMS:                result.Elapsed.Milliseconds(),
		BytesRead:                result.BytesRead,
		ThroughputBytesPerSecond: int64(result.ThroughputBytesPerSecond),
	}
	switch {
	case result.HTTPStatus != 0 && !successStatus(result.HTTPStatus):
		attempt.FailureCode = failureHTTPStatus
	case err != nil:
		attempt.FailureCode = classifyFailure(err)
	default:
		attempt.Reachable = true
	}
	return attempt
}
