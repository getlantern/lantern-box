package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"

	box "github.com/getlantern/lantern-box"
)

type tlsConfig struct {
	ServerName string
	DisableSNI bool
	Insecure   bool
	CertPath   string
	KeyPath    string
}

type config struct {
	Type       string
	Tag        string
	ListenPort uint16
	TLS        *tlsConfig
	User       map[string]any
}

func genOpts(cfg config) (option.Options, error) {
	ctx := box.BaseContext()
	inRegistery := service.FromContext[adapter.InboundRegistry](ctx)
	inbound, err := genInboundOpts(inRegistery, cfg)
	if err != nil {
		return option.Options{}, fmt.Errorf("inbound options: %w", err)
	}
	outRegistry := service.FromContext[adapter.OutboundRegistry](ctx)
	outbound, err := genOutboundOpts(outRegistry, cfg)
	if err != nil {
		return option.Options{}, fmt.Errorf("outbound options: %w", err)
	}

	options := option.Options{
		Inbounds:  []option.Inbound{inbound},
		Outbounds: []option.Outbound{outbound},
	}
	if _, err = json.MarshalContext(ctx, options); err != nil {
		return option.Options{}, fmt.Errorf("marshal options: %w", err)
	}
	return options, nil
}

func genInboundOpts(registry adapter.InboundRegistry, cfg config) (option.Inbound, error) {
	inbound := option.Inbound{
		Type: cfg.Type,
		Tag:  cfg.Tag,
	}
	opts, ok := registry.CreateOptions(cfg.Type)
	if !ok {
		return inbound, fmt.Errorf("unknown type: %s", cfg.Type)
	}
	options := reflect.ValueOf(opts).Elem()
	addr := badoption.Addr(netip.IPv4Unspecified())
	listenOpts := option.ListenOptions{
		Listen:     &addr,
		ListenPort: cfg.ListenPort,
	}
	if err := setField(options, "ListenOptions", listenOpts); err != nil {
		return inbound, fmt.Errorf("listen options: %w", err)
	}
	if cfg.TLS != nil {
		tls := tlsInOpts(cfg.TLS.ServerName, cfg.TLS.CertPath, cfg.TLS.KeyPath, cfg.TLS.Insecure)
		if err := setField(options, "TLS", tls); err != nil {
			if errors.Is(err, ErrNoSuchField) {
				return inbound, fmt.Errorf("%w: %s", ErrTLSNotSupported, cfg.Type)
			}
			return inbound, fmt.Errorf("tls options: %w", err)
		}
	}
	if len(cfg.User) > 0 {
		if err := setInboundUserValues(options, cfg.User); err != nil {
			return inbound, fmt.Errorf("user value: %w", err)
		}
	}
	inbound.Options = opts
	return inbound, nil
}

func genOutboundOpts(registry adapter.OutboundRegistry, cfg config) (option.Outbound, error) {
	outbound := option.Outbound{
		Type: cfg.Type,
		Tag:  cfg.Tag,
	}
	opts, ok := registry.CreateOptions(cfg.Type)
	if !ok {
		return outbound, fmt.Errorf("unknown type: %s", cfg.Type)
	}
	options := reflect.ValueOf(opts).Elem()
	serverOpts := option.ServerOptions{
		Server:     "localhost",
		ServerPort: 10000,
	}
	if err := setField(options, "ServerOptions", serverOpts); err != nil {
		return outbound, fmt.Errorf("server options: %w", err)
	}
	if cfg.TLS != nil {
		tls := tlsOutOpts(cfg.TLS.ServerName, cfg.TLS.CertPath, cfg.TLS.DisableSNI, cfg.TLS.Insecure)
		if err := setField(options, "TLS", tls); err != nil {
			if errors.Is(err, ErrNoSuchField) {
				return outbound, fmt.Errorf("%w: %s", ErrTLSNotSupported, cfg.Type)
			}
			return outbound, fmt.Errorf("tls options: %w", err)
		}
	}
	if len(cfg.User) > 0 {
		for fname, fvalue := range cfg.User {
			if err := setField(options, fname, fvalue); err != nil && !errors.Is(err, ErrNoSuchField) {
				return outbound, fmt.Errorf("user value %s: %w", fname, err)
			}
		}
	}
	outbound.Options = opts
	return outbound, nil
}

func checkValid(ctx context.Context, options option.Options) error {
	ctx, cancel := context.WithCancel(ctx)
	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: options,
	})
	if err == nil {
		instance.Close()
	}
	cancel()
	return err
}

var (
	ErrTLSNotSupported = errors.New("tls not supported")
	ErrNoSuchField     = errors.New("no such field")
	ErrTypeMismatch    = errors.New("type mismatch")
)

func setField(opts reflect.Value, field string, value any) error {
	f := opts.FieldByName(field)
	if !f.IsValid() {
		return fmt.Errorf("%w: %q", ErrNoSuchField, field)
	}
	v := reflect.ValueOf(value)
	if f.Type() != v.Type() {
		return fmt.Errorf("%w: expected %q but got %q", ErrTypeMismatch, f.Type().String(), v.Type().String())
	}
	f.Set(v)
	return nil
}

func hasField(opts reflect.Value, field string) bool {
	return opts.FieldByName(field).IsValid()
}

func setInboundUserValues(opts reflect.Value, values map[string]any) error {
	userField := opts.FieldByName("Users")
	if !userField.IsValid() {
		return fmt.Errorf("%w: \"Users\"", ErrNoSuchField)
	}
	user := reflect.New(userField.Type().Elem()).Elem()
	for fname, fvalue := range values {
		if err := setField(user, fname, fvalue); err != nil {
			return err
		}
	}
	userField.Set(reflect.Append(userField, user))
	return nil
}

func tlsInOpts(serverName, certPath, keyPath string, insecure bool) *option.InboundTLSOptions {
	return &option.InboundTLSOptions{
		Enabled:    true,
		ServerName: serverName,
		Insecure:   insecure,
		// ALPN:            badoption.Listable[string]{},
		// MinVersion:      "",
		// MaxVersion:      "",
		// CipherSuites:    badoption.Listable[string]{},
		// Certificate:     badoption.Listable[string]{},
		CertificatePath: certPath,
		// Key:             badoption.Listable[string]{},
		KeyPath: keyPath,
		// ACME:            &option.InboundACMEOptions{},
		// ECH:             &option.InboundECHOptions{},
		// Reality:         &option.InboundRealityOptions{},
	}
}

func tlsOutOpts(serverName, certPath string, disableSNI, insecure bool) *option.OutboundTLSOptions {
	return &option.OutboundTLSOptions{
		Enabled:    true,
		DisableSNI: disableSNI,
		ServerName: serverName,
		Insecure:   insecure,
		// ALPN:                  badoption.Listable[string]{},
		// MinVersion:            "",
		// MaxVersion:            "",
		// CipherSuites:          badoption.Listable[string]{},
		// Certificate:           badoption.Listable[string]{},
		CertificatePath: certPath,
		// Fragment:              false,
		// FragmentFallbackDelay: 0,
		// RecordFragment:        false,
		// ECH:                   &option.OutboundECHOptions{},
		// UTLS:                  &option.OutboundUTLSOptions{},
		// Reality:               &option.OutboundRealityOptions{},
	}
}
