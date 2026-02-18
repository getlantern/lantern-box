package unbounded

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	singlog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	C "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// WIP usage: edit sing-box/include/registry.go to import and register this protocol

type logAdapter struct {
	singBoxLogger singlog.ContextLogger
}

func (l logAdapter) Write(p []byte) (int, error) {
	l.singBoxLogger.Info(string(p))
	return len(p), nil
}

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.UnboundedOutboundOptions](registry, C.TypeUnbounded, NewOutbound)
}

type Outbound struct {
	outbound.Adapter
	logger       logger.ContextLogger
	broflakeConn *UBClientcore.BroflakeConn
	dial         UBClientcore.SOCKS5Dialer
	ui           UBClientcore.UI
	ql           *UBClientcore.QUICLayer
}

func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	logger singlog.ContextLogger,
	tag string,
	options option.UnboundedOutboundOptions,
) (adapter.Outbound, error) {
	logger.InfoContext(ctx, "Creating new unbounded outbound")
	bfOpt := UBClientcore.NewDefaultBroflakeOptions()
	if options.CTableSize != 0 {
		bfOpt.CTableSize = options.CTableSize
	}

	if options.PTableSize != 0 {
		bfOpt.PTableSize = options.PTableSize
	}

	if options.BusBufferSz != 0 {
		bfOpt.BusBufferSz = options.BusBufferSz
	}

	if options.Netstated != "" {
		bfOpt.Netstated = options.Netstated
	}

	rtcOpt := UBClientcore.NewDefaultWebRTCOptions()
	if options.DiscoverySrv != "" {
		rtcOpt.DiscoverySrv = options.DiscoverySrv
	}

	if options.DiscoveryEndpoint != "" {
		rtcOpt.Endpoint = options.DiscoveryEndpoint
	}

	if options.GenesisAddr != "" {
		rtcOpt.GenesisAddr = options.GenesisAddr
	}

	if options.NATFailTimeout != 0 {
		rtcOpt.NATFailTimeout = time.Duration(options.NATFailTimeout) * time.Second
	}

	if options.STUNBatchSize != 0 {
		rtcOpt.STUNBatchSize = uint32(options.STUNBatchSize)
	}

	if options.STUNBatch != nil {
		rtcOpt.STUNBatch = options.STUNBatch
	}

	if options.Tag != "" {
		rtcOpt.Tag = options.Tag
	}

	if options.Patience != 0 {
		rtcOpt.Patience = time.Duration(options.Patience) * time.Second
	}

	if options.ErrorBackoff != 0 {
		rtcOpt.ErrorBackoff = time.Duration(options.ErrorBackoff) * time.Second
	}

	if options.ConsumerSessionID != "" {
		rtcOpt.ConsumerSessionID = options.ConsumerSessionID
	}

	// XXX: This sing-box outbound implements a "desktop" type Unbounded peer, and
	// desktop peers don't connect to the egress server, so these egress settings
	// have no effect. We plumb them through here for the sake of future extensibility.
	egOpt := UBClientcore.NewDefaultEgressOptions()
	if options.EgressAddr != "" {
		egOpt.Addr = options.EgressAddr
	}

	if options.EgressEndpoint != "" {
		egOpt.Endpoint = options.EgressEndpoint
	}

	if options.EgressConnectTimeout != 0 {
		egOpt.ConnectTimeout = time.Duration(options.EgressConnectTimeout) * time.Second
	}

	if options.EgressErrorBackoff != 0 {
		egOpt.ErrorBackoff = time.Duration(options.EgressErrorBackoff) * time.Second
	}

	la := logAdapter{
		singBoxLogger: logger,
	}

	UBCommon.SetDebugLogger(log.New(la, "", 0))

	// wrap sing-box dialer in transport.Net for pion/webrtc usage
	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}
	rtcNet, err := newRTCNet(ctx, outboundDialer, logger)
	if err != nil {
		return nil, err
	}
	rtcOpt.Net = rtcNet

	/*
		outboundManager := service.FromContext[adapter.OutboundManager](ctx)
		direct, exists := outboundManager.Outbound(constant.TypeDirect)
		if !exists {
			slog.Error("Could not find direct outbound")
			return nil, errors.New("Could not find direct outbound")
		}
	*/
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Parse the address into host and port
		addr = strings.ReplaceAll(addr, "localhost", "127.0.0.1")
		conn, dialErr := rtcNet.dialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
		if dialErr != nil {
			logger.InfoContext(ctx, "unbounded error dialing context", "error", dialErr)
		} else {
			logger.InfoContext(ctx, "unbounded successfully connnected", "addr", addr)
		}
		return conn, err
	}
	rtcOpt.HTTPClient = &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				//return rtcNet.dialer.DialContext(ctx, network, addr)
				return dialContext(ctx, network, addr)
			},
			DialContext: dialContext,
		},
	}

	BFConn, ui, err := UBClientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
	if err != nil {
		logger.ErrorContext(ctx, "unbounded error creating new broflake", "error", err)
		return nil, err
	}

	// TODO: plumb through a real TLS cert and get rid of the self-signed generator?
	QUICLayer, err := UBClientcore.NewQUICLayer(BFConn, generateSelfSignedTLSConfig())
	if err != nil {
		logger.ErrorContext(ctx, "unbounded error creating new QUIC layer", "error", err)
		return nil, err
	}
	logger.InfoContext(ctx, "unbounded created broflake and QUIC layer")

	dialer := UBClientcore.CreateSOCKS5Dialer(QUICLayer)

	o := &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			C.TypeUnbounded,
			tag,
			[]string{N.NetworkTCP}, // XXX: Unbounded only supports TCP (not UDP) for now
			options.DialerOptions,
		),
		logger:       logger,
		broflakeConn: BFConn,
		dial:         dialer,
		ui:           ui,
		ql:           QUICLayer,
	}

	o.logger.Info("Created new unbounded outbound")
	return o, nil
}

func (h *Outbound) DialContext(
	ctx context.Context,
	network string,
	destination M.Socksaddr,
) (net.Conn, error) {
	// XXX: this is the log pattern for N.NetworkTCP
	h.logger.InfoContext(ctx, "unbounded outbound connection to ", destination)

	// XXX: network is ignored by Unbounded's SOCKS5 dialer
	return h.dial(ctx, network, destination.String())
}

func (h *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

func (h *Outbound) Start(stage adapter.StartStage) error {
	h.logger.Info("Starting unbounded outbound")
	if stage == adapter.StartStatePostStart {
		go h.ql.ListenAndMaintainQUICConnection()
	}
	return nil
}

func (h *Outbound) Close() error {
	h.ql.Close()
	h.ui.Stop()
	return nil
}

// TODO: delete me
func generateSelfSignedTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}

	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"broflake"},
	}
}
