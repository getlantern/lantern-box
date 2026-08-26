package twiddle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/log"
	boxoption "github.com/sagernet/sing-box/option"

	"github.com/getlantern/lantern-box/option"
	tw "github.com/getlantern/twiddle"
)

func creds(t *testing.T) (keyHex, ticketB64, pskHex string) {
	t.Helper()
	k, err := tw.NewTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := k.Issue(1, tw.DefaultTicketLen)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(c.Ticket), hex.EncodeToString(c.PSK[:])
}

func TestInboundRequiresMasqueradeUpstream(t *testing.T) {
	keyHex, _, _ := creds(t)
	_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{TicketKey: keyHex})
	if err == nil {
		t.Fatal("inbound accepted a config with no masquerade_upstream")
	}
	// twiddle cannot complete a real handshake with an unrecognised peer, so an
	// egress without a cover site hands probes a distinguishing reply.
	if !strings.Contains(err.Error(), "masquerade_upstream") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInboundRejectsBadTicketKey(t *testing.T) {
	for _, k := range []string{"", "zz", hex.EncodeToString(make([]byte, 16))} {
		_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
			option.TwiddleInboundOptions{TicketKey: k, MasqueradeUpstream: "www.example.com:443"})
		if err == nil {
			t.Errorf("inbound accepted ticket_key %q", k)
		}
	}
}

func TestInboundAcceptsAValidConfig(t *testing.T) {
	keyHex, _, _ := creds(t)
	ib, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleInboundOptions{
			TicketKey:          keyHex,
			MasqueradeUpstream: "www.example.com:443",
			TicketMaxAge:       "24h",
			TicketLen:          256,
		})
	if err != nil {
		t.Fatal(err)
	}
	if ib == nil {
		t.Fatal("nil inbound")
	}
}

func TestOutboundRequiresCoverSNI(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk,
		})
	if err == nil {
		t.Fatal("outbound accepted a config with no cover_sni")
	}
	if !strings.Contains(err.Error(), "cover_sni") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOutboundRejectsBadCredentials(t *testing.T) {
	_, ticket, psk := creds(t)
	bad := []option.TwiddleOutboundOptions{
		{Ticket: "not base64!!", PSK: psk, CoverSNI: "a.example"},
		{Ticket: ticket, PSK: "zz", CoverSNI: "a.example"},
		{Ticket: ticket, PSK: hex.EncodeToString(make([]byte, 16)), CoverSNI: "a.example"},
	}
	for i, o := range bad {
		o.ServerOptions = boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443}
		if _, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t", o); err == nil {
			t.Errorf("case %d: outbound accepted bad credentials", i)
		}
	}
}

func TestOutboundUsesTheEmbeddedPoolByDefault(t *testing.T) {
	_, ticket, psk := creds(t)
	ob, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "www.microsoft.com",
		})
	if err != nil {
		t.Fatal(err)
	}
	o := ob.(*Outbound)
	if len(o.cfg.Pool) == 0 {
		t.Fatal("outbound has an empty hello pool")
	}
	if o.cfg.CoverSNI != "www.microsoft.com" {
		t.Errorf("cover SNI is %q", o.cfg.CoverSNI)
	}
}

func TestOutboundRejectsACorruptPool(t *testing.T) {
	_, ticket, psk := creds(t)
	_, err := NewOutbound(context.Background(), nil, log.NewNOPFactory().Logger(), "t",
		option.TwiddleOutboundOptions{
			ServerOptions: boxoption.ServerOptions{Server: "127.0.0.1", ServerPort: 443},
			Ticket:        ticket, PSK: psk, CoverSNI: "a.example",
			HelloPool: "not hex at all",
		})
	if err == nil {
		t.Fatal("outbound accepted a corrupt hello pool")
	}
}
