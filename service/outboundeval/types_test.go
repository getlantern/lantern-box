package outboundeval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbO "github.com/getlantern/lantern-box/option"
)

type dualOutboundOptions struct {
	Server string `json:"server"`
}

type dualEndpointOptions struct {
	Peer string `json:"peer"`
}

// targetContext returns a context whose registries decode the targets these
// tests use. Its "dual" type is registered as both an outbound and an
// endpoint, each with options of its own, the way wireguard is.
func targetContext(parent context.Context) context.Context {
	errUnconstructed := errors.New("decode-only registration")
	outbounds := outbound.NewRegistry()
	outbound.Register[dualOutboundOptions](outbounds, "dual",
		func(context.Context, A.Router, log.ContextLogger, string, dualOutboundOptions) (A.Outbound, error) {
			return nil, errUnconstructed
		})
	outbound.Register[O.ShadowsocksOutboundOptions](outbounds, C.TypeShadowsocks,
		func(context.Context, A.Router, log.ContextLogger, string, O.ShadowsocksOutboundOptions) (A.Outbound, error) {
			return nil, errUnconstructed
		})
	endpoints := endpoint.NewRegistry()
	endpoint.Register[dualEndpointOptions](endpoints, "dual",
		func(context.Context, A.Router, log.ContextLogger, string, dualEndpointOptions) (A.Endpoint, error) {
			return nil, errUnconstructed
		})
	endpoint.Register[O.WireGuardEndpointOptions](endpoints, C.TypeWireGuard,
		func(context.Context, A.Router, log.ContextLogger, string, O.WireGuardEndpointOptions) (A.Endpoint, error) {
			return nil, errUnconstructed
		})
	ctx := service.ContextWith[O.OutboundOptionsRegistry](parent, outbounds)
	return service.ContextWith[O.EndpointOptionsRegistry](ctx, endpoints)
}

func TestEvaluationTargetTypeSelectsTheRegistry(t *testing.T) {
	ctx := targetContext(context.Background())
	var outboundTarget, endpointTarget EvaluationTarget

	require.NoError(t, sjson.UnmarshalContext(ctx,
		[]byte(`{"type":"outbound","options":{"type":"dual","server":"a"}}`), &outboundTarget))
	require.NoError(t, sjson.UnmarshalContext(ctx,
		[]byte(`{"type":"endpoint","options":{"type":"dual","peer":"b"}}`), &endpointTarget))

	assert.Equal(t, EvaluationTarget{Type: EvaluationTargetOutbound, Options: O.Outbound{
		Type: "dual", Options: &dualOutboundOptions{Server: "a"},
	}}, outboundTarget)
	assert.Equal(t, EvaluationTarget{Type: EvaluationTargetEndpoint, Options: O.Endpoint{
		Type: "dual", Options: &dualEndpointOptions{Peer: "b"},
	}}, endpointTarget)
}

func TestEvaluationTargetRoundTrips(t *testing.T) {
	ctx := targetContext(context.Background())
	for _, target := range []EvaluationTarget{
		{Type: EvaluationTargetOutbound, Options: O.Outbound{
			Type: "dual", Tag: "candidate", Options: &dualOutboundOptions{Server: "a"},
		}},
		{Type: EvaluationTargetEndpoint, Options: O.Endpoint{
			Type: "dual", Tag: "control", Options: &dualEndpointOptions{Peer: "b"},
		}},
	} {
		t.Run(target.Type, func(t *testing.T) {
			encoded, err := sjson.MarshalContext(ctx, &target)
			require.NoError(t, err)
			var decoded EvaluationTarget
			require.NoError(t, sjson.UnmarshalContext(ctx, encoded, &decoded))
			assert.Equal(t, target, decoded)
		})
	}
}

func TestEvaluationTargetRefusesWhatItCannotDecode(t *testing.T) {
	ctx := targetContext(context.Background())
	for name, content := range map[string]string{
		"unknown target type": `{"type":"inbound","options":{"type":"dual"}}`,
		"no target type":      `{"options":{"type":"dual"}}`,
		"unknown option type": `{"type":"outbound","options":{"type":"missing"}}`,
		"no options":          `{"type":"endpoint"}`,
		"numeric target type": `{"type":123,"options":{"type":"dual"}}`,
		"array wrapper":       `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			var target EvaluationTarget
			assert.ErrorIs(t, sjson.UnmarshalContext(ctx, []byte(content), &target), ErrInvalidContract)
		})
	}
}

var fixedNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func testBounds() bounds {
	return bounds{
		windows:           defaultMaxWindows,
		attemptsPerWindow: defaultMaxAttemptsPerWindow,
		responseBytes:     defaultMaxResponseBytes,
		assignmentBytes:   defaultMaxAssignmentBytes,
	}
}

func validAssignment() Assignment {
	return Assignment{
		ID:             "assignment-1",
		ReportToken:    "report-token",
		MeasurementURL: "https://measure.example/resource",
		Sample: SampleSpec{
			WindowsPerExit:        2,
			AttemptsPerWindow:     2,
			WindowDurationSeconds: 45,
			// The grid tests outside a synctest bubble wait this out for real.
			FreshSessionDelayMS: 10,
		},
		Challenges: []WindowChallenge{
			{WindowIndex: 0, Challenge: "challenge-0"},
			{WindowIndex: 1, Challenge: "challenge-1"},
		},
		ExpiresAt: fixedNow.Add(15 * time.Minute),
	}
}

func TestAssignmentValidateAcceptsAServerAssignment(t *testing.T) {
	for name, mutate := range map[string]func(*Assignment){
		"ordinary assignment": func(*Assignment) {},
		"long expiry":         func(a *Assignment) { a.ExpiresAt = fixedNow.Add(time.Hour) },
		"grid outlives the assignment": func(a *Assignment) {
			a.ExpiresAt = fixedNow.Add(time.Minute)
			a.Sample.WindowDurationSeconds = 120
		},
		"spacing outlives the assignment": func(a *Assignment) {
			a.ExpiresAt = fixedNow.Add(time.Minute)
			a.Sample.FreshSessionDelayMS = 120_000
		},
		"grid exceeds runtime budget": func(a *Assignment) {
			a.Sample.WindowsPerExit = defaultMaxWindows
			a.Sample.WindowDurationSeconds = maxWindowDurationSecs
			a.Challenges = make([]WindowChallenge, defaultMaxWindows)
			for i := range a.Challenges {
				a.Challenges[i] = WindowChallenge{WindowIndex: uint32(i), Challenge: "challenge"}
			}
		},
		"challenge out of range": func(a *Assignment) { a.Challenges[1].WindowIndex = 2 },
		"duplicate challenge":    func(a *Assignment) { a.Challenges[1].WindowIndex = 0 },
		"empty challenge":        func(a *Assignment) { a.Challenges[1].Challenge = "" },
	} {
		t.Run(name, func(t *testing.T) {
			assignment := validAssignment()
			mutate(&assignment)
			require.NoError(t, assignment.validate(fixedNow, testBounds()))
		})
	}
}

func TestAssignmentValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(*Assignment){
		"no windows":           func(a *Assignment) { a.Sample.WindowsPerExit = 0 },
		"too many windows":     func(a *Assignment) { a.Sample.WindowsPerExit = 9 },
		"no attempts":          func(a *Assignment) { a.Sample.AttemptsPerWindow = 0 },
		"too many attempts":    func(a *Assignment) { a.Sample.AttemptsPerWindow = 9 },
		"no window duration":   func(a *Assignment) { a.Sample.WindowDurationSeconds = 0 },
		"long window duration": func(a *Assignment) { a.Sample.WindowDurationSeconds = maxWindowDurationSecs + 1 },
		"long fresh delay":     func(a *Assignment) { a.Sample.FreshSessionDelayMS = maxFreshSessionDelayMS + 1 },
		"already expired":      func(a *Assignment) { a.ExpiresAt = fixedNow.Add(-time.Second) },
		"expires now":          func(a *Assignment) { a.ExpiresAt = fixedNow },
		"missing expiry":       func(a *Assignment) { a.ExpiresAt = time.Time{} },
		"missing a challenge":  func(a *Assignment) { a.Challenges = a.Challenges[:1] },
		"extra challenge": func(a *Assignment) {
			a.Challenges = append(a.Challenges, WindowChallenge{WindowIndex: 2, Challenge: "challenge-2"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			assignment := validAssignment()
			mutate(&assignment)
			err := assignment.validate(fixedNow, testBounds())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidContract)
		})
	}
}

func TestAssignmentValidateRefusesAnEndpointThatListens(t *testing.T) {
	endpointTarget := func(endpointType string, options any) *EvaluationTarget {
		return &EvaluationTarget{Type: EvaluationTargetEndpoint, Options: O.Endpoint{
			Type: endpointType, Options: options,
		}}
	}
	amnezia := func(wireguard O.WireGuardEndpointOptions) *lbO.AmneziaEndpointOptions {
		return &lbO.AmneziaEndpointOptions{WireGuardEndpointOptions: wireguard}
	}
	for name, test := range map[string]struct {
		target  *EvaluationTarget
		refused bool
	}{
		"outbound": {target: &EvaluationTarget{Type: EvaluationTargetOutbound, Options: O.Outbound{
			Type: "dual", Options: &dualOutboundOptions{},
		}}},
		"wireguard": {target: endpointTarget(C.TypeWireGuard, &O.WireGuardEndpointOptions{})},
		"amnezia":   {target: endpointTarget("amnezia", amnezia(O.WireGuardEndpointOptions{}))},
		"wireguard listening": {
			target:  endpointTarget(C.TypeWireGuard, &O.WireGuardEndpointOptions{ListenPort: 51820}),
			refused: true,
		},
		"wireguard system interface": {
			target:  endpointTarget(C.TypeWireGuard, &O.WireGuardEndpointOptions{System: true}),
			refused: true,
		},
		"amnezia listening": {
			target:  endpointTarget("amnezia", amnezia(O.WireGuardEndpointOptions{ListenPort: 51820})),
			refused: true,
		},
		"amnezia system interface": {
			target:  endpointTarget("amnezia", amnezia(O.WireGuardEndpointOptions{System: true})),
			refused: true,
		},
		"endpoint type it cannot check": {
			target:  endpointTarget("dual", &dualEndpointOptions{}),
			refused: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, control := validAssignment(), validAssignment()
			candidate.Candidate = test.target
			control.Control = test.target
			for _, assignment := range []Assignment{candidate, control} {
				err := assignment.validate(fixedNow, testBounds())
				if test.refused {
					assert.ErrorIs(t, err, ErrInvalidContract)
				} else {
					assert.NoError(t, err)
				}
			}
		})
	}
}

// The candidate target is proxied, so the worst case of a grid is the user's data
// the service would spend.
func TestAssignmentValidateRefusesAGridThatSpendsTooMuchData(t *testing.T) {
	assignment := validAssignment()
	assignment.Sample.WindowsPerExit = defaultMaxWindows
	assignment.Sample.AttemptsPerWindow = defaultMaxAttemptsPerWindow
	assignment.Challenges = make([]WindowChallenge, defaultMaxWindows)
	for i := range assignment.Challenges {
		assignment.Challenges[i] = WindowChallenge{WindowIndex: uint32(i), Challenge: "challenge"}
	}

	err := assignment.validate(fixedNow, testBounds())

	require.ErrorIs(t, err, ErrInvalidContract)
	assert.ErrorContains(t, err, "bytes")

	roomier := testBounds()
	roomier.assignmentBytes = assignment.Sample.maxBytes(roomier.responseBytes)
	assert.NoError(t, assignment.validate(fixedNow, roomier))
}

// requireCompleteGrid asserts the report fills the sample's grid exactly,
// every attempt carrying a failure code when and only when it is unreachable.
func requireCompleteGrid(t *testing.T, report Report, sample SampleSpec) {
	t.Helper()
	require.Len(t, report.Windows, int(sample.WindowsPerExit))
	for _, window := range report.Windows {
		require.Len(t, window.CandidateAttempts, int(sample.AttemptsPerWindow))
		require.Len(t, window.ControlAttempts, int(sample.AttemptsPerWindow))
		for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
			for _, attempt := range attempts {
				require.NotEqual(t, attempt.Reachable, attempt.FailureCode != "")
			}
		}
	}
}

// The fixtures are the schema the control API implements against, so a change
// that breaks them is a change to that contract.
func TestFixturesRoundTrip(t *testing.T) {
	for name, target := range map[string]any{
		"assignment.json":          &Assignment{},
		"attestation_request.json": &AttestationRequest{},
		"attestation.json":         &Attestation{},
		"report.json":              &Report{},
		"report_padded.json":       &Report{},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, target))

			encoded, err := json.Marshal(target)
			require.NoError(t, err)
			var want, got any
			require.NoError(t, json.Unmarshal(raw, &want))
			require.NoError(t, json.Unmarshal(encoded, &got))
			assert.Equal(t, want, got)
		})
	}
}

// An assignment carrying its own pair decodes only in a registry context, so
// it is round-tripped through sing's context JSON rather than with the other
// fixtures.
func TestEvaluationTargetsFixtureRoundTrips(t *testing.T) {
	ctx := targetContext(context.Background())
	raw, err := os.ReadFile(filepath.Join("testdata", "assignment_targets.json"))
	require.NoError(t, err)
	var assignment Assignment
	require.NoError(t, sjson.UnmarshalContext(ctx, raw, &assignment))
	require.NoError(t, assignment.validate(fixedNow, testBounds()))
	require.Equal(t, EvaluationTargetOutbound, assignment.Candidate.Type)
	require.Equal(t, EvaluationTargetEndpoint, assignment.Control.Type)

	encoded, err := sjson.MarshalContext(ctx, &assignment)
	require.NoError(t, err)
	var want, got any
	require.NoError(t, json.Unmarshal(raw, &want))
	require.NoError(t, json.Unmarshal(encoded, &got))
	assert.Equal(t, want, got)
}

func TestFixturedAssignmentIsMeasurable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "assignment.json"))
	require.NoError(t, err)
	var assignment Assignment
	require.NoError(t, json.Unmarshal(raw, &assignment))
	require.NoError(t, assignment.validate(fixedNow, testBounds()))
}

func TestFixturedReportsFillTheGrid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "assignment.json"))
	require.NoError(t, err)
	var assignment Assignment
	require.NoError(t, json.Unmarshal(raw, &assignment))

	for _, name := range []string{"report.json", "report_padded.json"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			require.NoError(t, err)
			var report Report
			require.NoError(t, json.Unmarshal(raw, &report))
			requireCompleteGrid(t, report, assignment.Sample)
			for _, window := range report.Windows {
				require.NotEmpty(t, window.AttestationToken)
			}
		})
	}
}
