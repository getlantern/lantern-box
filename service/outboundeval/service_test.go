package outboundeval

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

// dialOutbound stands in for one arm, connecting to a fixed test address
// whatever destination it is handed.
type dialOutbound struct {
	A.Outbound
	tag     string
	address string
}

func (o *dialOutbound) Tag() string { return o.tag }

func (o *dialOutbound) DialContext(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, o.address)
}

type stubOutboundManager struct {
	A.OutboundManager
	outbounds map[string]A.Outbound
}

func (m *stubOutboundManager) Outbound(tag string) (A.Outbound, bool) {
	out, found := m.outbounds[tag]
	return out, found
}

func contextWithOutbounds(outbounds map[string]A.Outbound) context.Context {
	return service.ContextWith[A.OutboundManager](
		context.Background(), &stubOutboundManager{outbounds: outbounds},
	)
}

// wiredService is a service pointed at a stub control API, wired as Start would
// wire it but without the cycle loop running.
func wiredService(t *testing.T, handler http.HandlerFunc) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	options := testOptions()
	options.AcquireURL = server.URL + "/assignments"
	options.AttestURL = server.URL + "/attestations"
	options.SubmitURL = server.URL + "/reports"

	control := &dialOutbound{tag: "direct", address: server.Listener.Addr().String()}
	candidate := &dialOutbound{tag: "candidate", address: server.Listener.Addr().String()}
	ctx := contextWithOutbounds(map[string]A.Outbound{"direct": control, "candidate": candidate})

	created, err := NewService(ctx, log.NewNOPFactory().Logger(), "eval", options)
	require.NoError(t, err)
	s := created.(*Service)
	s.retryDelay = time.Millisecond
	s.outbounds = service.FromContext[A.OutboundManager](ctx)
	s.control = control
	s.api = newAPIClient(ctx, control, time.Duration(s.options.RequestTimeout))
	s.attest = s.api.attest
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s, server
}

func TestNewServiceAppliesDefaults(t *testing.T) {
	s := newTestService(t, testOptions())

	assert.Equal(t, defaultControlOutboundTag, s.options.ControlOutboundTag)
	assert.Equal(t, defaultPollInterval, time.Duration(s.options.PollInterval))
	assert.Equal(t, defaultNoAssignmentInterval, time.Duration(s.options.NoAssignmentInterval))
	assert.Equal(t, defaultMaxRetryBackoff, time.Duration(s.options.MaxRetryBackoff))
	assert.Equal(t, defaultRequestTimeout, time.Duration(s.options.RequestTimeout))
	assert.EqualValues(t, defaultMaxResponseBytes, s.options.MaxResponseBytes)
	assert.EqualValues(t, defaultMaxAssignmentBytes, s.options.MaxAssignmentBytes)
	assert.EqualValues(t, defaultMaxWindows, s.options.MaxWindows)
	assert.EqualValues(t, defaultMaxAttemptsPerWindow, s.options.MaxAttemptsPerWindow)
}

func TestNewServiceRejectsUnusableOptions(t *testing.T) {
	for name, mutate := range map[string]func(*option.OutboundEvalServiceOptions){
		"no acquire url": func(o *option.OutboundEvalServiceOptions) { o.AcquireURL = "" },
		"no attest url":  func(o *option.OutboundEvalServiceOptions) { o.AttestURL = "" },
		"no submit url":  func(o *option.OutboundEvalServiceOptions) { o.SubmitURL = "" },
		"plaintext to the world": func(o *option.OutboundEvalServiceOptions) {
			o.AcquireURL = "http://control.example/assignments"
		},
		"unsupported scheme": func(o *option.OutboundEvalServiceOptions) {
			o.SubmitURL = "ftp://control.example/reports"
		},
		"url with credentials": func(o *option.OutboundEvalServiceOptions) {
			o.AttestURL = "https://user:pw@control.example/attestations"
		},
		"relative url":    func(o *option.OutboundEvalServiceOptions) { o.AcquireURL = "/assignments" },
		"no country":      func(o *option.OutboundEvalServiceOptions) { o.CountryCode = "" },
		"long country":    func(o *option.OutboundEvalServiceOptions) { o.CountryCode = "RUS" },
		"numeric country": func(o *option.OutboundEvalServiceOptions) { o.CountryCode = "R1" },
		"no outbound":     func(o *option.OutboundEvalServiceOptions) { o.OutboundTag = "" },
	} {
		t.Run(name, func(t *testing.T) {
			options := testOptions()
			mutate(&options)
			_, err := NewService(context.Background(), log.NewNOPFactory().Logger(), "eval", options)
			assert.Error(t, err)
		})
	}
}

func TestNewServiceAcceptsAPlaintextLoopbackAPI(t *testing.T) {
	options := testOptions()
	options.AcquireURL = "http://localhost:8080/assignments"
	_, err := NewService(context.Background(), log.NewNOPFactory().Logger(), "eval", options)
	assert.NoError(t, err)
}

func TestStartRequiresBothArms(t *testing.T) {
	candidate := &stubOutbound{tag: "candidate"}
	direct := &stubOutbound{tag: "direct"}
	for name, outbounds := range map[string]map[string]A.Outbound{
		"no control outbound":    {"candidate": candidate},
		"no outbound under test": {"direct": direct},
		"no outbounds at all":    {},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := contextWithOutbounds(outbounds)
			created, err := NewService(ctx, log.NewNOPFactory().Logger(), "eval", testOptions())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, created.Close()) })
			assert.Error(t, created.Start(A.StartStateStart))
		})
	}
}

func TestStartWithoutAnOutboundManager(t *testing.T) {
	created, err := NewService(context.Background(), log.NewNOPFactory().Logger(), "eval", testOptions())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, created.Close()) })
	assert.Error(t, created.Start(A.StartStateStart))
}

func TestStartMeasuresNothingBeforeItsStage(t *testing.T) {
	requests := 0
	s, _ := wiredService(t, func(http.ResponseWriter, *http.Request) { requests++ })

	for _, stage := range []A.StartStage{A.StartStateInitialize, A.StartStatePostStart, A.StartStateStarted} {
		require.NoError(t, s.Start(stage))
	}
	assert.False(t, s.started.Load())
	assert.Zero(t, requests)
}

func TestCloseIsIdempotentAndPromptWithoutAStart(t *testing.T) {
	s := newTestService(t, testOptions())

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, s.Close())
		require.NoError(t, s.Close())
	}()
	select {
	case <-done:
	case <-time.After(closeGracePeriod):
		t.Fatal("Close blocked on a cycle that never ran")
	}
}

func TestSetOutboundEvalConfigReplacesTheRunningConfig(t *testing.T) {
	s := newTestService(t, testOptions())

	require.NoError(t, s.SetOutboundEvalConfig(lbA.OutboundEvalConfig{
		Token: "rotated", CountryCode: "ir", OutboundTag: "replacement",
	}))

	assert.Equal(t, lbA.OutboundEvalConfig{
		Token: "rotated", CountryCode: "IR", OutboundTag: "replacement",
	}, *s.config.Load())
}

func TestSetOutboundEvalConfigAcceptsNoCredentialYet(t *testing.T) {
	s := newTestService(t, testOptions())

	require.NoError(t, s.SetOutboundEvalConfig(lbA.OutboundEvalConfig{
		CountryCode: "RU", OutboundTag: "candidate",
	}))
	assert.Empty(t, s.config.Load().Token)
}

func TestSetOutboundEvalConfigRejectsAnUnusableConfig(t *testing.T) {
	s := newTestService(t, testOptions())
	before := *s.config.Load()

	for name, config := range map[string]lbA.OutboundEvalConfig{
		"no country":   {Token: "t", OutboundTag: "candidate"},
		"long country": {Token: "t", CountryCode: "RUS", OutboundTag: "candidate"},
		"no outbound":  {Token: "t", CountryCode: "RU"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, s.SetOutboundEvalConfig(config))
			assert.Equal(t, before, *s.config.Load())
		})
	}
}

func TestSetOutboundEvalConfigWakesAWaitingCycle(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		name := "polling"
		if backoff {
			name = "backoff"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var tokens []string
				s := startTestRunner(t, func(w http.ResponseWriter, r *http.Request) {
					tokens = append(tokens, r.Header.Get("Authorization"))
					w.WriteHeader(http.StatusBadGateway)
				})
				if backoff {
					s.wake <- struct{}{}
				}
				synctest.Wait()
				before := len(tokens)
				if backoff {
					require.Equal(t, 1, before)
				} else {
					require.Zero(t, before)
				}
				require.NoError(t, s.SetOutboundEvalConfig(lbA.OutboundEvalConfig{
					Token: "rotated", CountryCode: "RU", OutboundTag: "candidate",
				}))
				synctest.Wait()
				require.Len(t, tokens, before+1)
				assert.Equal(t, "Bearer rotated", tokens[before])
			})
		})
	}
}
