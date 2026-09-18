package outboundeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		ExpiresAt:  fixedNow.Add(15 * time.Minute),
		ServerTime: fixedNow,
	}
}

func TestAssignmentValidateAcceptsAServerAssignment(t *testing.T) {
	require.NoError(t, validAssignment().validate(fixedNow, testBounds()))
}

func TestAssignmentValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(*Assignment){
		"no id":                   func(a *Assignment) { a.ID = "" },
		"no report token":         func(a *Assignment) { a.ReportToken = "" },
		"oversized report token":  func(a *Assignment) { a.ReportToken = string(make([]byte, maxTokenBytes+1)) },
		"plaintext measurement":   func(a *Assignment) { a.MeasurementURL = "http://measure.example/x" },
		"relative measurement":    func(a *Assignment) { a.MeasurementURL = "/resource" },
		"measurement credentials": func(a *Assignment) { a.MeasurementURL = "https://user:pw@measure.example/x" },
		"measurement fragment":    func(a *Assignment) { a.MeasurementURL = "https://measure.example/x#frag" },
		"no windows":              func(a *Assignment) { a.Sample.WindowsPerExit = 0 },
		"too many windows":        func(a *Assignment) { a.Sample.WindowsPerExit = 9 },
		"no attempts":             func(a *Assignment) { a.Sample.AttemptsPerWindow = 0 },
		"too many attempts":       func(a *Assignment) { a.Sample.AttemptsPerWindow = 9 },
		"no window duration":      func(a *Assignment) { a.Sample.WindowDurationSeconds = 0 },
		"long window duration":    func(a *Assignment) { a.Sample.WindowDurationSeconds = maxWindowDurationSecs + 1 },
		"long fresh delay":        func(a *Assignment) { a.Sample.FreshSessionDelayMS = maxFreshSessionDelayMS + 1 },
		"no server time":          func(a *Assignment) { a.ServerTime = time.Time{} },
		"already expired":         func(a *Assignment) { a.ExpiresAt = fixedNow.Add(-time.Second) },
		"expiry too far out":      func(a *Assignment) { a.ExpiresAt = fixedNow.Add(maxAssignmentTTL + time.Minute) },
		"missing a challenge":     func(a *Assignment) { a.Challenges = a.Challenges[:1] },
		"grid outlives the assignment": func(a *Assignment) {
			a.ExpiresAt = fixedNow.Add(time.Minute)
			a.Sample.WindowDurationSeconds = 120
		},
		"spacing outlives the assignment": func(a *Assignment) {
			a.ExpiresAt = fixedNow.Add(time.Minute)
			a.Sample.FreshSessionDelayMS = 120_000
		},
		"grid spends more data than allowed": func(a *Assignment) {
			a.Sample.WindowsPerExit = defaultMaxWindows
			a.Sample.AttemptsPerWindow = defaultMaxAttemptsPerWindow
			a.Challenges = make([]WindowChallenge, defaultMaxWindows)
			for i := range a.Challenges {
				a.Challenges[i] = WindowChallenge{WindowIndex: uint32(i), Challenge: "challenge"}
			}
		},
		"challenge for a second exit": func(a *Assignment) { a.Challenges[1].ExitIndex = 1 },
		"challenge out of range":      func(a *Assignment) { a.Challenges[1].WindowIndex = 2 },
		"duplicate challenge":         func(a *Assignment) { a.Challenges[1].WindowIndex = 0 },
		"empty challenge":             func(a *Assignment) { a.Challenges[1].Challenge = "" },
		"oversized challenge": func(a *Assignment) {
			a.Challenges[1].Challenge = string(make([]byte, maxChallengeBytes+1))
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

// The candidate arm is proxied, so the worst case of a grid is the user's data
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

func completeReport(sample SampleSpec) Report {
	report := Report{
		AssignmentID:   "assignment-1",
		ReportToken:    "report-token",
		IdempotencyKey: "assignment-1",
		ObservedAt:     fixedNow,
	}
	for window := uint32(0); window < sample.WindowsPerExit; window++ {
		report.Windows = append(report.Windows, WindowReport{
			WindowIndex:       window,
			AttestationToken:  "attestation",
			CandidateAttempts: reachableAttempts(int(sample.AttemptsPerWindow)),
			ControlAttempts:   reachableAttempts(int(sample.AttemptsPerWindow)),
		})
	}
	return report
}

func reachableAttempts(count int) []Attempt {
	attempts := make([]Attempt, count)
	for i := range attempts {
		attempts[i] = Attempt{Reachable: true, HTTPStatus: 200, BytesRead: 1024}
	}
	return attempts
}

func TestReportValidateRequiresTheWholeGrid(t *testing.T) {
	sample := validAssignment().Sample
	require.NoError(t, completeReport(sample).validate(sample))

	for name, mutate := range map[string]func(*Report){
		"a window short": func(r *Report) { r.Windows = r.Windows[:1] },
		"a candidate attempt short": func(r *Report) {
			r.Windows[1].CandidateAttempts = r.Windows[1].CandidateAttempts[:1]
		},
		"a control attempt short": func(r *Report) {
			r.Windows[1].ControlAttempts = r.Windows[1].ControlAttempts[:1]
		},
		"a repeated window":        func(r *Report) { r.Windows[1].WindowIndex = 0 },
		"a window out of range":    func(r *Report) { r.Windows[1].WindowIndex = 7 },
		"a window of another exit": func(r *Report) { r.Windows[1].ExitIndex = 1 },
		"reachable with a failure code": func(r *Report) {
			r.Windows[0].CandidateAttempts[0].FailureCode = failureTimeout
		},
		"unreachable without one": func(r *Report) {
			r.Windows[0].ControlAttempts[0].Reachable = false
		},
		"upper-case failure code": func(r *Report) {
			r.Windows[0].CandidateAttempts[0] = Attempt{FailureCode: "Timeout"}
		},
		"oversized failure code": func(r *Report) {
			r.Windows[0].CandidateAttempts[0] = Attempt{FailureCode: string(make([]byte, maxFailureCodeBytes+1))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := completeReport(sample)
			mutate(&report)
			err := report.validate(sample)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidContract)
		})
	}
}

func TestReportValidateAcceptsAPaddedWindow(t *testing.T) {
	sample := validAssignment().Sample
	report := completeReport(sample)
	report.Windows[1] = fillWindow(
		WindowReport{WindowIndex: 1}, sample, failureAttestation,
	)
	require.NoError(t, report.validate(sample))
	assert.Empty(t, report.Windows[1].AttestationToken)
}

// The fixtures are the schema the control API implements against, so a change
// that breaks them is a change to that contract.
func TestFixturesRoundTrip(t *testing.T) {
	for name, target := range map[string]any{
		"assignment.json":    &Assignment{},
		"attestation.json":   &Attestation{},
		"report.json":        &Report{},
		"report_padded.json": &Report{},
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

func TestFixturedAssignmentIsMeasurable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "assignment.json"))
	require.NoError(t, err)
	var assignment Assignment
	require.NoError(t, json.Unmarshal(raw, &assignment))
	require.NoError(t, assignment.validate(assignment.ServerTime, testBounds()))
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
			require.NoError(t, report.validate(assignment.Sample))
		})
	}
}
