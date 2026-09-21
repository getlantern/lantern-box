// Package outboundeval measures one outbound against the un-proxied path and
// reports the pair to a control API.
//
// A cycle asks the control API for an assignment, proves which address it is
// measuring from, fetches the assigned resource through both arms as many times
// as the assignment asks for, and submits the result. An assignment names the
// resource and the sample, and nothing about what is under test.
package outboundeval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/ntp"
	"github.com/sagernet/sing/service"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// Defaults for every option left unset.
const (
	defaultControlOutboundTag   = "direct"
	defaultPollInterval         = 5 * time.Minute
	defaultNoAssignmentInterval = 5 * time.Minute
	defaultMaxRetryBackoff      = 10 * time.Minute
	defaultRequestTimeout       = 30 * time.Second
	defaultMaxResponseBytes     = 1 << 20
	defaultMaxAssignmentBytes   = 32 << 20
	defaultMaxWindows           = 8
	defaultMaxAttemptsPerWindow = 8
)

// closeGracePeriod is how long Close waits for the cycle to unwind. It stays
// under sing-box's stop timeout so a measurement in flight cannot fail box
// shutdown.
const closeGracePeriod = 3 * time.Second

// RegisterService adds the outbound evaluation service to a sing-box service
// registry.
func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.OutboundEvalServiceOptions](registry, constant.TypeOutboundEval, NewService)
}

// Service is the measurement runner. It measures nothing until the box reaches
// A.StartStateStart, which is the first stage at which every outbound it needs
// exists.
type Service struct {
	boxService.Adapter

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	logger log.ContextLogger

	options option.OutboundEvalServiceOptions
	limits  bounds

	config atomic.Pointer[lbA.OutboundEvalConfig]
	// pendingAssignment is owned by the run loop and retained across acquisition retries.
	pendingAssignment *pendingAssignment
	// wake carries a configuration change to a cycle waiting to retry, so a
	// fresh credential does not have to wait out a backoff.
	wake      chan struct{}
	started   atomic.Bool
	closeOnce sync.Once

	outbounds A.OutboundManager
	control   A.Outbound
	api       *apiClient
	// timeService keeps the clock a report is judged and stamped against. Start
	// resolves it before anything reads it.
	timeService ntp.TimeService

	// retryDelay is the gap between the bounded retries inside one cycle, and
	// retryBase the base wait for a retried cycle; both are shortened in tests.
	retryDelay time.Duration
	retryBase  time.Duration

	// measure and attest are the network-facing steps, replaced in tests that
	// exercise grid assembly without a network.
	measure func(ctx context.Context, out A.Outbound, target string) Attempt
	attest  func(ctx context.Context, request AttestationRequest) (Attestation, error)
}

var (
	_ A.Service                    = (*Service)(nil)
	_ lbA.OutboundEvalConfigSetter = (*Service)(nil)
)

// NewService builds the runner from its options, resolving no outbound and
// making no request until Start reaches A.StartStateStart.
func NewService(
	ctx context.Context,
	logger log.ContextLogger,
	tag string,
	options option.OutboundEvalServiceOptions,
) (A.Service, error) {
	options = withDefaults(options)
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &Service{
		Adapter: boxService.NewAdapter(constant.TypeOutboundEval, tag),
		ctx:     runCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		logger:  logger,
		options: options,
		wake:    make(chan struct{}, 1),

		retryDelay: defaultRetryDelay,
		retryBase:  retryBaseWait,
	}
	s.limits = bounds{
		windows:           options.MaxWindows,
		attemptsPerWindow: options.MaxAttemptsPerWindow,
		responseBytes:     options.MaxResponseBytes,
		assignmentBytes:   options.MaxAssignmentBytes,
	}
	s.config.Store(&lbA.OutboundEvalConfig{
		Token:       options.Token,
		CountryCode: strings.ToUpper(options.CountryCode),
		OutboundTag: options.OutboundTag,
	})
	s.measure = s.measureOnce
	return s, nil
}

func (s *Service) Start(stage A.StartStage) error {
	if stage != A.StartStateStart {
		return nil
	}
	outbounds := service.FromContext[A.OutboundManager](s.ctx)
	if outbounds == nil {
		return errors.New("no outbound manager in context")
	}
	control, found := outbounds.Outbound(s.options.ControlOutboundTag)
	if !found {
		return fmt.Errorf("control outbound %q is not declared in this config",
			s.options.ControlOutboundTag)
	}
	candidateTag := s.config.Load().OutboundTag
	if _, found := outbounds.Outbound(candidateTag); !found {
		return fmt.Errorf("outbound under test %q is not declared in this config",
			candidateTag)
	}
	if timeService := service.FromContext[ntp.TimeService](s.ctx); timeService != nil {
		s.timeService = timeService
	} else {
		// Cloning the registry keeps this clock off the box's own, where
		// autoselect and urltest would judge TLS validity against it.
		s.ctx = service.ExtendContext(s.ctx)
		server := M.ParseSocksaddr(s.options.NTPServer)
		ntpDialer, err := dialer.New(s.ctx, O.DialerOptions{}, server.IsDomain())
		if err != nil {
			return fmt.Errorf("build ntp dialer: %w", err)
		}
		ntpService := ntp.NewService(ntp.Options{
			Context: s.ctx,
			Dialer:  ntpDialer,
			Logger:  s.logger,
			Server:  server,
		})
		if err := ntpService.Start(); err != nil {
			return fmt.Errorf("start time service: %w", err)
		}
		// probe.Measure resolves its TLS clock from the context rather than
		// from this service, so the measurement arms need it registered too.
		service.MustRegister[ntp.TimeService](s.ctx, ntpService)
		s.timeService = ntpService
	}
	s.outbounds = outbounds
	s.control = control
	s.api = newAPIClient(s.ctx, control, s.timeService.TimeFunc(), s.options)
	s.attest = s.api.attest
	s.started.Store(true)
	go s.run()
	return nil
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.started.Load() {
			select {
			case <-s.done:
			case <-time.After(closeGracePeriod):
				s.logger.Warn("outbound evaluation did not stop within ", closeGracePeriod)
			}
		}
		if s.api != nil {
			s.api.close()
		}
	})
	return nil
}

// SetOutboundEvalConfig implements adapter.OutboundEvalConfigSetter. It rejects
// a configuration it cannot act on, leaving the running one untouched.
func (s *Service) SetOutboundEvalConfig(config lbA.OutboundEvalConfig) error {
	if config.OutboundTag == "" {
		return errors.New("outbound evaluation: outbound under test is required")
	}
	config.CountryCode = strings.ToUpper(config.CountryCode)
	s.config.Store(&config)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func withDefaults(options option.OutboundEvalServiceOptions) option.OutboundEvalServiceOptions {
	if options.ControlOutboundTag == "" {
		options.ControlOutboundTag = defaultControlOutboundTag
	}
	if options.PollInterval <= 0 {
		options.PollInterval = badoption.Duration(defaultPollInterval)
	}
	if options.NoAssignmentInterval <= 0 {
		options.NoAssignmentInterval = badoption.Duration(defaultNoAssignmentInterval)
	}
	if options.MaxRetryBackoff <= 0 {
		options.MaxRetryBackoff = badoption.Duration(defaultMaxRetryBackoff)
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = badoption.Duration(defaultRequestTimeout)
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = defaultMaxResponseBytes
	}
	if options.MaxAssignmentBytes <= 0 {
		options.MaxAssignmentBytes = defaultMaxAssignmentBytes
	}
	if options.MaxWindows == 0 {
		options.MaxWindows = defaultMaxWindows
	}
	if options.MaxAttemptsPerWindow == 0 {
		options.MaxAttemptsPerWindow = defaultMaxAttemptsPerWindow
	}
	return options
}

func validateOptions(options option.OutboundEvalServiceOptions) error {
	if options.AcquireURL == "" {
		return errors.New("acquire_url is required")
	}
	if options.AttestURL == "" {
		return errors.New("attest_url is required")
	}
	if options.SubmitURL == "" {
		return errors.New("submit_url is required")
	}
	if options.OutboundTag == "" {
		return errors.New("outbound_tag is required")
	}
	return nil
}
