//nolint:paralleltest // Tests share the process-wide diagnostic recorder.
package connectiondiag_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/getlantern/lantern-box/connectiondiag"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/socketobserver"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/database64128/tfo-go/v2"
)

func TestSharedDialerDiagnostics(t *testing.T) {
	connectiondiag.Enable(false)
	before, err := connectiondiag.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var prior struct {
		Total uint64 `json:"total_dials"`
	}
	if err := json.Unmarshal(before, &prior); err != nil {
		t.Fatal(err)
	}
	d, err := dialer.NewDefault(socketobserver.WithLabel(connectiondiag.Context(context.Background()), "socks", "test"), option.DialerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", M.ParseSocksaddr(ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if c, err := d.DialContext(canceled, "tcp", M.ParseSocksaddr(ln.Addr().String())); err == nil {
		c.Close()
		t.Fatal("canceled dial succeeded")
	}
	pc, err := d.ListenPacket(ctx, M.ParseSocksaddr("127.0.0.1:12345"))
	if err != nil {
		t.Fatal(err)
	}
	pc.Close()
	data, err := connectiondiag.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Records []connectiondiag.Record `json:"records"`
	}
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	records := v.Records[:0]
	for _, record := range v.Records {
		if record.ID > prior.Total {
			records = append(records, record)
		}
	}
	v.Records = records
	if len(v.Records) != 3 {
		t.Fatalf("expected one record per socket attempt, got %s", data)
	}
	if !v.Records[0].Closed || v.Records[0].TCP == nil {
		t.Fatalf("missing TCP stats: %s", data)
	}
	if v.Records[1].Error != "canceled" || v.Records[1].ErrorStage != "connect" {
		t.Fatalf("missing dial failure: %s", data)
	}
	if v.Records[2].TCPStatus != "not_tcp" || v.Records[2].TCP != nil {
		t.Fatalf("UDP must not claim TCP stats: %s", data)
	}
}

func TestFastOpenRecordsActualDial(t *testing.T) {
	connectiondiag.Enable(false)
	before, _ := connectiondiag.Snapshot()
	var prior struct {
		Total uint64 `json:"total_dials"`
	}
	json.Unmarshal(before, &prior)
	ctx, cancel := context.WithCancel(socketobserver.WithLabel(connectiondiag.Context(context.Background()), "socks", "tfo"))
	cancel()
	c, err := dialer.DialSlowContext(&tfo.Dialer{}, ctx, "tcp", M.ParseSocksaddr("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pending, _ := connectiondiag.Snapshot()
	var interim struct {
		Total uint64 `json:"total_dials"`
	}
	json.Unmarshal(pending, &interim)
	if interim.Total != prior.Total {
		t.Fatal("lazy socket reported a premature dial")
	}
	if _, err = c.Write([]byte("hello")); err == nil {
		t.Fatal("canceled fast-open dial succeeded")
	}
	data, _ := connectiondiag.Snapshot()
	var result struct {
		Total   uint64                  `json:"total_dials"`
		Records []connectiondiag.Record `json:"records"`
	}
	json.Unmarshal(data, &result)
	if result.Total != prior.Total+1 {
		t.Fatalf("expected one actual dial: %s", data)
	}
	last := result.Records[len(result.Records)-1]
	if last.Error != "canceled" || last.ErrorStage != "connect" {
		t.Fatalf("incorrect fast-open failure: %+v", last)
	}
}
