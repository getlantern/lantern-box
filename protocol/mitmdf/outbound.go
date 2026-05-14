// Package mitmdf implements a MITM-DomainFronting sing-box outbound.
//
// The outbound terminates the user's TLS handshake locally using a leaf
// certificate minted on the fly by a name-constrained CA (see
// internal/mitmca). Once the user side completes, the configured front
// table is consulted to choose a fronted SNI and (optionally) a redirect
// destination. The egress connection is wrapped with uTLS using the
// configured fingerprint preset and the user's negotiated ALPN — never
// the preset's hard-coded ALPN list, which is the load-bearing piece for
// transparently fronting both h2 and http/1.1 origins. Plaintext is then
// bridged between the two TLS sides.
//
// Security properties (full rationale in
// docs/mitm-df/architecture.md and getlantern/engineering#3482):
//
//   - The CA carries RFC 5280 NameConstraints, so even a leaked CA key
//     can't mint certs outside the configured permitted_domains.
//   - The outbound never listens on a TCP port — the user side of each
//     MITM session is an in-memory net.Pipe owned by DialContext.
//   - A deny list rejects sensitive SNIs in the GetCertificate callback
//     before any signature is produced.
//   - Every decision (allow / deny / no-match / error) is recorded to a
//     JSONL audit log.
package mitmdf

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	utls "github.com/refraction-networking/utls"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/internal/mitmca"
	"github.com/getlantern/lantern-box/option"
)

const defaultEgressHandshakeTimeout = 10 * time.Second

// RegisterOutbound registers the mitm-df outbound adapter.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.MITMDFOutboundOptions](registry, constant.TypeMITMDF, NewOutbound)
}

// Outbound is a MITM-DomainFronting outbound.
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	dialer N.Dialer

	ca      *mitmca.CA
	fronts  fronts
	deny    []string
	audit   *auditLog
	helloID utls.ClientHelloID

	egressHandshakeTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewOutbound constructs a mitm-df outbound from parsed options.
func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	lg sblog.ContextLogger,
	tag string,
	opts option.MITMDFOutboundOptions,
) (adapter.Outbound, error) {
	ca, err := loadOrGenerateCA(opts.CA, lg, ctx)
	if err != nil {
		return nil, err
	}
	fronts, err := buildFronts(opts.Fronts)
	if err != nil {
		return nil, err
	}
	helloID, err := presetFromString(opts.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("mitmdf: %w", err)
	}
	handshakeTimeout := defaultEgressHandshakeTimeout
	if opts.EgressHandshakeTimeout != "" {
		handshakeTimeout, err = time.ParseDuration(opts.EgressHandshakeTimeout)
		if err != nil {
			return nil, fmt.Errorf("mitmdf: invalid egress_handshake_timeout: %w", err)
		}
		if handshakeTimeout <= 0 {
			return nil, fmt.Errorf("mitmdf: egress_handshake_timeout must be positive")
		}
	}

	outboundDialer, err := dialer.New(ctx, opts.DialerOptions, opts.ServerIsDomain())
	if err != nil {
		return nil, fmt.Errorf("mitmdf: build dialer: %w", err)
	}

	audit, err := openAuditLog(opts.AuditLogPath)
	if err != nil {
		return nil, fmt.Errorf("mitmdf: %w", err)
	}

	o := &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			constant.TypeMITMDF, tag, []string{N.NetworkTCP}, opts.DialerOptions),
		logger:                 lg,
		dialer:                 outboundDialer,
		ca:                     ca,
		fronts:                 fronts,
		deny:                   normalizeDeny(opts.DenyDomains),
		audit:                  audit,
		helloID:                helloID,
		egressHandshakeTimeout: handshakeTimeout,
	}
	o.ctx, o.cancel = context.WithCancel(context.Background())
	return o, nil
}

// loadOrGenerateCA loads an existing CA from disk if both cert_path and
// the keystore have data, otherwise generates a fresh one scoped to the
// configured permitted_domains and persists it.
func loadOrGenerateCA(opts option.MITMDFCAOptions, lg sblog.ContextLogger, ctx context.Context) (*mitmca.CA, error) {
	if opts.CertPath == "" || opts.KeyPath == "" {
		return nil, errors.New("mitmdf: ca.cert_path and ca.key_path are required")
	}
	if len(opts.PermittedDomains) == 0 {
		return nil, errors.New("mitmdf: ca.permitted_domains is required (refusing to operate without NameConstraints)")
	}

	keystore := mitmca.NewFileKeyStore(opts.KeyPath)

	certBytes, certReadErr := os.ReadFile(opts.CertPath)
	_, keyStatErr := os.Stat(opts.KeyPath)
	switch {
	case certReadErr == nil && keyStatErr == nil:
		ca, err := mitmca.LoadCA(certBytes, keystore)
		if err != nil {
			return nil, fmt.Errorf("mitmdf: load CA: %w", err)
		}
		logCAFingerprint(lg, ctx, ca, "loaded")
		return ca, nil
	case os.IsNotExist(certReadErr) || os.IsNotExist(keyStatErr):
		validity := mitmca.DefaultCAValidity
		if opts.Validity != "" {
			d, err := time.ParseDuration(opts.Validity)
			if err != nil {
				return nil, fmt.Errorf("mitmdf: invalid ca.validity: %w", err)
			}
			if d <= 0 {
				return nil, errors.New("mitmdf: ca.validity must be positive")
			}
			validity = d
		}
		if err := os.MkdirAll(filepath.Dir(opts.CertPath), 0o700); err != nil {
			return nil, fmt.Errorf("mitmdf: create CA cert dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(opts.KeyPath), 0o700); err != nil {
			return nil, fmt.Errorf("mitmdf: create CA key dir: %w", err)
		}
		ca, err := mitmca.GenerateCA(opts.PermittedDomains, keystore, validity)
		if err != nil {
			return nil, fmt.Errorf("mitmdf: generate CA: %w", err)
		}
		if err := writeAtomic(opts.CertPath, ca.CertPEM(), 0o644); err != nil {
			return nil, fmt.Errorf("mitmdf: persist CA cert: %w", err)
		}
		logCAFingerprint(lg, ctx, ca, "generated")
		return ca, nil
	default:
		return nil, fmt.Errorf("mitmdf: probe CA paths: cert=%v key=%v", certReadErr, keyStatErr)
	}
}

func logCAFingerprint(lg sblog.ContextLogger, ctx context.Context, ca *mitmca.CA, action string) {
	sum := sha256.Sum256(ca.Cert().Raw)
	lg.InfoContext(ctx,
		"mitmdf: CA ", action, " — SHA-256 ", hex.EncodeToString(sum[:]),
		" — permitted_domains=", ca.PermittedDomains(),
		" — expires ", ca.NotAfter().Format(time.RFC3339),
		" — install the cert on user devices for the user-side TLS handshake to succeed")
}

// DialContext returns a synthetic net.Conn whose Read/Write are the user
// side of an in-memory pipe. A background goroutine drives the user-side
// tls.Server handshake (with leaf cert minted via the GetCertificate
// callback against the actual ClientHello SNI), dials the matched front,
// performs the uTLS handshake, and bridges plaintext.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	if N.NetworkName(network) != N.NetworkTCP {
		return nil, fmt.Errorf("mitmdf: only TCP is supported (got %q)", network)
	}

	userSide, proxySide := net.Pipe()
	o.wg.Add(1)
	go o.serve(ctx, destination, proxySide)
	return userSide, nil
}

// ListenPacket is not supported; mitm-df is TCP-only.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("mitmdf: UDP not supported: %w", os.ErrInvalid)
}

// Network reports the supported network types.
func (o *Outbound) Network() []string {
	return []string{N.NetworkTCP}
}

// Close cancels in-flight serve goroutines, waits for them to drain, and
// closes the audit log.
func (o *Outbound) Close() error {
	o.cancel()
	o.wg.Wait()
	return o.audit.close()
}

// serve runs the MITM pipeline for a single dial. The SNI is taken from
// the user's ClientHello — not from the router-supplied destination — via
// the GetCertificate callback below, so a routing/sniff mismatch can't
// mint a cert for the wrong name.
func (o *Outbound) serve(
	ctx context.Context,
	destination M.Socksaddr,
	proxySide net.Conn,
) {
	defer o.wg.Done()
	defer proxySide.Close()

	dialCtx, cancel := context.WithCancel(o.ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-stop:
		}
	}()

	// State captured by the GetCertificate closure so the post-handshake
	// front-selection step has the same answers without re-evaluating.
	var (
		matchedSNI   string
		matchedFront *frontEntry
	)
	serverCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni := normalizeDNS(hello.ServerName)
			if sni == "" {
				o.audit.record(auditRecord{
					SNI: hello.ServerName, Decision: auditDeny,
					Reason: "empty SNI",
				})
				return nil, errors.New("mitmdf: client did not send SNI")
			}
			if denyMatch(o.deny, sni) {
				o.audit.record(auditRecord{
					SNI: sni, Decision: auditDeny, Reason: "deny list",
				})
				return nil, fmt.Errorf("mitmdf: SNI %q is on the deny list", sni)
			}
			front := o.fronts.lookup(sni)
			if front == nil {
				o.audit.record(auditRecord{
					SNI: sni, Decision: auditNoMatch,
				})
				return nil, fmt.Errorf("mitmdf: SNI %q is not claimed by any fronts entry", sni)
			}
			leafCert, leafKey, err := o.ca.SignLeaf(sni)
			if err != nil {
				o.audit.record(auditRecord{
					SNI: sni, FrontedSNI: front.frontedSNI, Decision: auditError,
					Reason: "sign leaf: " + err.Error(),
				})
				return nil, fmt.Errorf("mitmdf: sign leaf: %w", err)
			}
			o.audit.record(auditRecord{
				SNI: sni, FrontedSNI: front.frontedSNI, Decision: auditAllow,
			})
			matchedSNI = sni
			matchedFront = front
			return &tls.Certificate{
				Certificate: [][]byte{leafCert.Raw, o.ca.Cert().Raw},
				PrivateKey:  leafKey,
				Leaf:        leafCert,
			}, nil
		},
	}
	userTLS := tls.Server(proxySide, serverCfg)
	defer userTLS.Close()

	hsCtx, hsCancel := context.WithTimeout(dialCtx, o.egressHandshakeTimeout)
	if err := userTLS.HandshakeContext(hsCtx); err != nil {
		hsCancel()
		o.logger.DebugContext(ctx, "mitmdf: user TLS handshake failed: ", err,
			" (dest=", destination.String(), ")")
		return
	}
	hsCancel()
	if matchedFront == nil {
		// Defensive — handshake completing without our callback firing
		// would imply a session resumption or TLS-internal bug; refuse to
		// proceed.
		o.logger.WarnContext(ctx, "mitmdf: handshake completed without GetCertificate match; aborting")
		return
	}
	negotiatedALPN := userTLS.ConnectionState().NegotiatedProtocol

	egressTarget := destination
	if matchedFront.redirectAddr != "" {
		egressTarget = M.ParseSocksaddr(matchedFront.redirectAddr)
	}

	rawEgress, err := o.dialer.DialContext(dialCtx, N.NetworkTCP, egressTarget)
	if err != nil {
		o.audit.record(auditRecord{
			SNI: matchedSNI, FrontedSNI: matchedFront.frontedSNI, Decision: auditError,
			Reason: "egress dial: " + err.Error(),
		})
		o.logger.DebugContext(ctx, "mitmdf: egress TCP dial failed to ",
			egressTarget.String(), ": ", err)
		return
	}
	defer rawEgress.Close()

	egressALPN := []string{negotiatedALPN}
	if negotiatedALPN == "" {
		egressALPN = []string{"h2", "http/1.1"}
	}
	uCfg := &utls.Config{
		ServerName: matchedFront.frontedSNI,
		NextProtos: egressALPN,
		MinVersion: utls.VersionTLS12,
	}
	if len(matchedFront.verifySAN) > 0 {
		uCfg.InsecureSkipVerify = true
		uCfg.VerifyPeerCertificate = verifyFrontSAN(matchedFront.frontedSNI, matchedFront.verifySAN)
	}
	uConn := utls.UClient(rawEgress, uCfg, utls.HelloCustom)
	defer uConn.Close()
	if err := applyPresetWithALPN(uConn, o.helloID, egressALPN); err != nil {
		o.logger.DebugContext(ctx, "mitmdf: apply preset with ALPN: ", err)
		return
	}

	egCtx, egCancel := context.WithTimeout(dialCtx, o.egressHandshakeTimeout)
	if err := uConn.HandshakeContext(egCtx); err != nil {
		egCancel()
		o.audit.record(auditRecord{
			SNI: matchedSNI, FrontedSNI: matchedFront.frontedSNI, Decision: auditError,
			Reason: "egress handshake: " + err.Error(),
		})
		o.logger.DebugContext(ctx, "mitmdf: egress uTLS handshake failed (sni=",
			matchedFront.frontedSNI, "): ", err)
		return
	}
	egCancel()

	bridge(userTLS, uConn)
}

// bridge proxies plaintext between user and egress. When either direction
// returns, both connections are closed to unblock the other copy.
func bridge(userTLS, egressTLS net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	closeBoth := func() {
		userTLS.Close()
		egressTLS.Close()
	}
	go func() {
		defer wg.Done()
		_, _ = io.Copy(egressTLS, userTLS)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(userTLS, egressTLS)
		closeBoth()
	}()
	wg.Wait()
}

// verifyFrontSAN returns a VerifyPeerCertificate callback that accepts the
// egress leaf when it is valid for frontedSNI or any of extraNames. Chain
// verification uses the system root pool (which is the right default for
// CDN edges); InsecureSkipVerify must be true on the *utls.Config when
// this callback is installed, since we re-implement both chain + name
// checking here.
func verifyFrontSAN(frontedSNI string, extraNames []string) func([][]byte, [][]*x509.Certificate) error {
	candidates := append([]string{frontedSNI}, extraNames...)
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("mitmdf: egress peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("mitmdf: parse egress leaf: %w", err)
		}
		intermediates := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				continue
			}
			intermediates.AddCert(c)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Intermediates: intermediates}); err != nil {
			return fmt.Errorf("mitmdf: egress chain verify: %w", err)
		}
		for _, name := range candidates {
			if name == "" {
				continue
			}
			if leaf.VerifyHostname(name) == nil {
				return nil
			}
		}
		return fmt.Errorf("mitmdf: egress cert valid for none of %v", candidates)
	}
}

// writeAtomic writes data to path via a tempfile + rename. Mode is applied
// to the tempfile before rename so the final file's mode is correct
// regardless of the umask. Parent directory is created upstream.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
