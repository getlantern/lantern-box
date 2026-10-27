package group

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	sbAdapter "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getlantern/lantern-box/option"
)

const (
	helperTagA = "helper-0123456789ab"
	helperTagB = "helper-abcdefabcdef"
)

func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exporter
}

// assertNoHelperHex fails if any span name, attribute, event or status in
// spans carries a helper tag's hex suffix.
func assertNoHelperHex(t *testing.T, spans tracetest.SpanStubs) {
	t.Helper()
	require.NotEmpty(t, spans)
	check := func(where, s string) {
		for _, hex := range []string{"0123456789ab", "abcdefabcdef"} {
			assert.NotContains(t, s, hex, "%s exports a helper tag", where)
		}
	}
	checkAttrs := func(where string, attrs []attribute.KeyValue) {
		for _, kv := range attrs {
			check(fmt.Sprintf("%s attribute %s", where, kv.Key), kv.Value.Emit())
		}
	}
	for _, s := range spans {
		check("span name", s.Name)
		check(s.Name+" status", s.Status.Description)
		checkAttrs(s.Name, s.Attributes)
		for _, e := range s.Events {
			check(s.Name+" event name", e.Name)
			checkAttrs(s.Name+" event "+e.Name, e.Attributes)
		}
	}
}

func spanAttr(spans tracetest.SpanStubs, name string, key attribute.Key) (string, bool) {
	for _, s := range spans {
		if s.Name != name {
			continue
		}
		for _, kv := range s.Attributes {
			if kv.Key == key {
				return kv.Value.Emit(), true
			}
		}
	}
	return "", false
}

func helperDialErr(tag string) error {
	return fmt.Errorf("outbound/unbounded[%s]: %w", tag, assert.AnError)
}

func TestMutableSelectorSpans_RedactHelperTags(t *testing.T) {
	exporter := recordSpans(t)

	ob := &mockOutbound{tag: helperTagA}
	ob.On("DialContext").Return(nil, helperDialErr(helperTagA))
	ob.On("ListenPacket").Return(nil, helperDialErr(helperTagA))
	ctx := service.ContextWith[sbAdapter.OutboundManager](context.Background(),
		&mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{helperTagA: ob}})
	out, err := NewMutableSelector(ctx, nil, log.NewNOPFactory().Logger(), "selector",
		option.MutableSelectorOutboundOptions{Outbounds: []string{helperTagA}})
	require.NoError(t, err)
	s := out.(*MutableSelector)
	require.NoError(t, s.Start())
	require.Equal(t, helperTagA, s.Now())

	_, err = s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.Error(t, err)
	_, err = s.ListenPacket(context.Background(), metadata.Socksaddr{})
	require.Error(t, err)

	// No selected outbound, so NewConnection fails at once, but its span
	// still carries the routed connection's outbound tag.
	empty := &MutableSelector{logger: log.NewNOPFactory().Logger()}
	empty.NewConnection(context.Background(), nil, sbAdapter.InboundContext{Outbound: helperTagB}, nil)
	empty.NewPacketConnection(context.Background(), nil, sbAdapter.InboundContext{Outbound: helperTagB}, nil)

	spans := exporter.GetSpans()
	assertNoHelperHex(t, spans)
	v, ok := spanAttr(spans, "MutableSelector.DialContext", "outbound")
	require.True(t, ok)
	assert.Equal(t, "helper", v)
	v, ok = spanAttr(spans, "MutableSelector.NewConnection", "outbound")
	require.True(t, ok)
	assert.Equal(t, "helper", v)
}

func TestMutableURLTestSpans_RedactHelperTags(t *testing.T) {
	for _, mode := range retryModes {
		t.Run(mode.method, func(t *testing.T) {
			exporter := recordSpans(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			s, outbounds := newRetryRig(mode, ctx,
				map[string]uint16{helperTagA: 50, helperTagB: 1000}, helperTagA)
			for tag, ob := range outbounds {
				ob.On(mode.method).Return(nil, helperDialErr(tag))
			}
			_, err := mode.invoke(s, ctx)
			require.Error(t, err)

			spans := exporter.GetSpans()
			assertNoHelperHex(t, spans)
			v, ok := spanAttr(spans, "MutableURLTest."+mode.method, "outbound")
			require.True(t, ok)
			assert.Equal(t, "helper", v)
			var failEvents int
			for _, sp := range spans {
				for _, e := range sp.Events {
					if strings.HasSuffix(e.Name, "_failed") {
						failEvents++
					}
				}
			}
			assert.Equal(t, 2, failEvents, "one failure event per helper outbound tried")
		})
	}
}

func TestMutableAutoSelectSpans_RedactHelperTags(t *testing.T) {
	exporter := recordSpans(t)
	s, obs := newTestMUR(t, helperTagA)
	recordSuccess(s, helperTagA, 50)
	obs[helperTagA].On("DialContext").Return(&net.TCPConn{}, nil)
	obs[helperTagA].On("ListenPacket").Return(&net.UDPConn{}, nil)

	// The first dial makes the helper outbound sticky, so the next spans
	// carry it as "outbound".
	for range 2 {
		_, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		_, err = s.ListenPacket(context.Background(), metadata.Socksaddr{})
		require.NoError(t, err)
	}

	spans := exporter.GetSpans()
	assertNoHelperHex(t, spans)
	v, ok := spanAttr(spans[len(spans)-2:], "MutableAutoSelect.DialContext", "outbound")
	require.True(t, ok)
	assert.Equal(t, "helper", v)
}
