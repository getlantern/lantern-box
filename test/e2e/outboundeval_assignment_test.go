package e2e

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lboption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/service/outboundeval"
)

func TestOutboundEvalCreatesAndRemovesAssignmentOutbounds(t *testing.T) {
	var manager A.OutboundManager
	reported := make(chan outboundeval.Report, 1)
	measured := make(chan struct{}, 2)
	port := proxyPort(t)
	assignment := outboundeval.Assignment{
		ID: "assignment", ReportToken: "report",
		// A tag the box's own outbound already uses, which the service must not replace.
		Candidate: proxyTarget("direct", port),
		Control:   proxyTarget("proxy-in", port),
		Sample: outboundeval.SampleSpec{
			WindowsPerExit: 1, AttemptsPerWindow: 1, WindowDurationSeconds: 5,
		},
		Challenges: []outboundeval.WindowChallenge{{Challenge: "challenge"}},
		ExpiresAt:  time.Now().Add(time.Minute),
	}
	ctx := evalBoxContext()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assignments":
			// The standard library would drop the targets' sing-box options.
			encoded, err := sjson.MarshalContext(ctx, assignment)
			assert.NoError(t, err)
			_, err = w.Write(encoded)
			assert.NoError(t, err)
		case "/attestations":
			assert.Empty(t, r.Header.Get("Authorization"))
			assert.NoError(t, json.NewEncoder(w).Encode(outboundeval.Attestation{Token: "attested"}))
		case "/measure":
			assert.Len(t, manager.Outbounds(), 3)
			measured <- struct{}{}
			_, err := w.Write([]byte("measurement"))
			assert.NoError(t, err)
		case "/reports":
			assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
			assert.Len(t, manager.Outbounds(), 1)
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
	options := evalBoxOptions(server.URL, "token", directOutbound(), port)
	options.Certificate = &option.CertificateOptions{
		Certificate: []string{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))},
	}
	options.Services[0].Options.(*lboption.OutboundEvalServiceOptions).PollInterval = badoption.Duration(time.Second)
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: options})
	require.NoError(t, err)
	manager = service.FromContext[A.OutboundManager](ctx)
	configuredControl, found := manager.Outbound("direct")
	require.True(t, found)
	require.NoError(t, instance.Start())
	defer func() { require.NoError(t, instance.Close()) }()

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
	currentControl, _ := manager.Outbound("direct")
	assert.Same(t, configuredControl, currentControl)
	assert.Len(t, manager.Outbounds(), 1)
}
