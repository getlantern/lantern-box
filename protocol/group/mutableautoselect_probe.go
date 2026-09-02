package group

import (
	"context"
	"sync"
	"time"

	A "github.com/sagernet/sing-box/adapter"

	"github.com/getlantern/lantern-box/internal/probe"
)

type probeResult struct {
	tag     string
	success bool
	delayMs uint32
}

func probeMember(
	ctx context.Context,
	out A.Outbound,
	probeURL string,
	beh protocolBehavior,
) probeResult {
	tag := out.Tag()
	if beh.excludeFromPool {
		return probeResult{tag: tag}
	}
	res := probe.Run(ctx, out, probeURL, beh.probeTimeout)
	if !res.Success {
		return probeResult{tag: tag}
	}
	// 1ms floor so a sub-millisecond probe isn't reported as 0; rank
	// treats delay==0 as "no recent success" and would drop the winner.
	delayMs := uint32(res.Delay / time.Millisecond)
	if delayMs == 0 {
		delayMs = 1
	}
	return probeResult{tag: tag, success: true, delayMs: delayMs}
}

// probeAll runs jobs with up to probeConcurrency workers, records each
// outcome, and calls onSuccess serially for successful probes. It returns
// after all queued probes complete or ctx is canceled.
func (s *MutableAutoSelect) probeAll(
	ctx context.Context,
	jobs []probeJob,
	onSuccess func(res probeResult),
) {
	if len(jobs) == 0 {
		return
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		queue   = make(chan probeJob)
		workers = max(1, min(len(jobs), s.cfg.probeConcurrency))
	)
	for range workers {
		wg.Go(func() {
			for j := range queue {
				res := probeMember(ctx, j.outbound, j.probeURL, j.beh)
				// Batch cancellation (shutdown or ladder budget) is not member
				// evidence. Per-probe timeouts use a child context, so the
				// batch ctx remains live and the failure still counts.
				if ctx.Err() != nil {
					continue
				}
				s.recordProbeOutcome(res.tag, res.success, res.delayMs)
				if !res.success || onSuccess == nil {
					continue
				}
				mu.Lock()
				onSuccess(res)
				mu.Unlock()
			}
		})
	}
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return
		}
	}
	close(queue)
	wg.Wait()
}
