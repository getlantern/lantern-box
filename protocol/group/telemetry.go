package group

import (
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/lantern-box/adapter"
)

// tagAttr returns a span attribute carrying an outbound tag, with any helper
// tag redacted: a paired inviter's helper- tags must not leave the client.
func tagAttr(key, tag string) attribute.KeyValue {
	return attribute.String(key, adapter.RedactHelperTags(tag))
}

// recordError records err on span with any helper tag in its message
// redacted, since dial errors can name the outbound that failed.
func recordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	if msg := err.Error(); adapter.RedactHelperTags(msg) != msg {
		err = errors.New(adapter.RedactHelperTags(msg))
	}
	span.RecordError(err)
}
