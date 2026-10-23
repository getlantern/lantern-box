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
	delay, err := probe.Run(ctx, out, probeURL, beh.probeTimeout)
	if err != nil {
		// Every failure is reported as a failed probe, an unusable probe
		// URL included: separating those would change which members demote.
		return probeResult{tag: tag}
	}
	// 1ms floor so a sub-millisecond probe isn't reported as 0; rank
	// treats delay==0 as "no recent success" and would drop the winner.
	delayMs := uint32(delay / time.Millisecond)
	if delayMs == 0 {
		delayMs = 1
	}
	return probeResult{tag: tag, success: true, delayMs: delayMs}
}

// probeWithWatchdog probes j's member and returns its result, or a failed
// result once probeBound passes or ctx ends while the outbound's dial is still
// running. An abandoned probe keeps its outbound in abandonedProbes until the
// dial returns, and its late result is discarded.
func (s *MutableAutoSelect) probeWithWatchdog(ctx context.Context, j probeJob) probeResult {
	done := make(chan probeResult, 1)
	go func() { done <- probeMember(ctx, j.outbound, j.probeURL, j.beh) }()
	watchdog := time.NewTimer(probeBound(j.beh))
	defer watchdog.Stop()
	select {
	case res := <-done:
		return res
	case <-watchdog.C:
	case <-ctx.Done():
	}
	s.access.Lock()
	if s.abandonedProbes == nil {
		s.abandonedProbes = make(map[A.Outbound]bool)
	}
	s.abandonedProbes[j.outbound] = true
	s.access.Unlock()
	go func() {
		<-done
		s.access.Lock()
		delete(s.abandonedProbes, j.outbound)
		s.access.Unlock()
	}()
	return probeResult{tag: j.outbound.Tag()}
}

// probeAll runs jobs with up to probeConcurrency workers, records each
// outcome, and calls onSuccess serially for successful probes. It doesn't wait
// past ctx, or past probeBound for any one probe, even when an outbound ignores
// ctx.
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
				res := s.probeWithWatchdog(ctx, j)
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
