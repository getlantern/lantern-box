package box_test

import (
	"context"
	"encoding/json"
	"testing"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/connectiondiag"
	"github.com/sagernet/sing-box/common/socketobserver"
)

func TestContextInstallsConnectionDiagnostics(t *testing.T) {
	connectiondiag.Enable(false)
	for _, build := range []func(context.Context) context.Context{box.Context, func(context.Context) context.Context { return box.BaseContext() }} {
		ctx := socketobserver.WithLabel(build(context.Background()), "context-test", "registration")
		done := socketobserver.FromContext(ctx).Begin("tcp", "example.invalid:443")
		if done == nil {
			t.Fatal("box context did not install collector")
		}
		done(nil, context.Canceled)
	}
	data, err := connectiondiag.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Records []connectiondiag.Record `json:"records"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range result.Records {
		if r.Protocol == "context-test" && r.Error == "canceled" {
			count++
		}
	}
	if count < 2 {
		t.Fatalf("missing installed-observer records: %s", data)
	}
}
