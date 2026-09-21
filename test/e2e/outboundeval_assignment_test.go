package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	lboption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/service/outboundeval"
)

func TestOutboundEvalCreatesAndRemovesAssignmentOutbounds(t *testing.T) {
	var manager A.OutboundManager
	reported := make(chan outboundeval.Report, 1)
	measured := make(chan struct{}, 2)
	assignment := outboundeval.Assignment{
		ID: "assignment", ReportToken: "report",
		Candidate: &option.Outbound{Type: C.TypeDirect, Tag: "candidate", Options: &option.DirectOutboundOptions{}},
		Control:   &option.Outbound{Type: C.TypeDirect, Tag: "direct", Options: &option.DirectOutboundOptions{}},
		Sample: outboundeval.SampleSpec{
			WindowsPerExit: 1, AttemptsPerWindow: 1, WindowDurationSeconds: 5,
		},
		Challenges: []outboundeval.WindowChallenge{{Challenge: "challenge"}},
		ExpiresAt:  time.Now().Add(time.Minute),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assignments":
			assert.NoError(t, json.NewEncoder(w).Encode(assignment))
		case "/attestations":
			assert.Empty(t, r.Header.Get("Authorization"))
			assert.NoError(t, json.NewEncoder(w).Encode(outboundeval.Attestation{Token: "attested"}))
		case "/measure":
			assert.Len(t, manager.Outbounds(), 4)
			measured <- struct{}{}
			_, err := w.Write([]byte("measurement"))
			assert.NoError(t, err)
		case "/reports":
			assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
			assert.Len(t, manager.Outbounds(), 2)
			var report outboundeval.Report
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode report: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			reported <- report
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	assignment.MeasurementURL = server.URL + "/measure"
	ctx := evalBoxContext()
	options := evalBoxOptions(server.URL, "token", bothArms())
	options.Services[0].Options.(*lboption.OutboundEvalServiceOptions).PollInterval = badoption.Duration(time.Hour)
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: options})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	defer func() { require.NoError(t, instance.Close()) }()
	manager = service.FromContext[A.OutboundManager](ctx)
	configuredCandidate, found := manager.Outbound("candidate")
	require.True(t, found)
	configuredControl, found := manager.Outbound("direct")
	require.True(t, found)
	evaluation, found := service.FromContext[A.ServiceManager](ctx).Get("eval")
	require.True(t, found)
	require.NoError(t, evaluation.(lbA.OutboundEvalConfigSetter).SetOutboundEvalConfig(lbA.OutboundEvalConfig{
		Token: "token", CountryCode: "RU", OutboundTag: "candidate",
	}))

	select {
	case report := <-reported:
		require.Len(t, report.Windows, 1)
		window := report.Windows[0]
		assert.Equal(t, "attested", window.AttestationToken)
		require.Len(t, window.CandidateAttempts, 1)
		require.Len(t, window.ControlAttempts, 1)
		assert.True(t, window.CandidateAttempts[0].Reachable)
		assert.True(t, window.ControlAttempts[0].Reachable)
	case <-time.After(10 * time.Second):
		t.Fatal("assignment report was not submitted")
	}
	assert.Len(t, measured, 2)
	currentCandidate, _ := manager.Outbound("candidate")
	currentControl, _ := manager.Outbound("direct")
	assert.Same(t, configuredCandidate, currentCandidate)
	assert.Same(t, configuredControl, currentControl)
	assert.Len(t, manager.Outbounds(), 2)
}
