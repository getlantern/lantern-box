package amnezia

import (
	"context"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/sagernet/sing-box/protocol/wireguard"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/log"
	F "github.com/sagernet/sing/common/format"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterEndpoint(registry *endpoint.Registry) {
	endpoint.Register[option.AmneziaEndpointOptions](registry, constant.TypeAmnezia, NewEndpoint)
}

var (
	_ adapter.Endpoint = (*Endpoint)(nil)
)

type Endpoint struct {
	*wireguard.Endpoint
}

func NewEndpoint(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.AmneziaEndpointOptions) (adapter.Endpoint, error) {
	ep, err := wireguard.NewEndpoint(ctx, router, logger, tag, options.WireGuardEndpointOptions)
	if err != nil {
		return nil, err
	}
	wg := ep.(*wireguard.Endpoint)

	// add amnezia specific options to ipcConf
	v := reflect.ValueOf(wg).Elem()
	endpointField := v.FieldByName("endpoint")
	if !endpointField.IsValid() {
		return nil, fmt.Errorf("field 'endpoint' not found")
	}
	endpointPtr := endpointField.Pointer()
	if endpointPtr == 0 {
		return nil, fmt.Errorf("endpoint is nil")
	}
	wgTransportType := endpointField.Type().Elem()
	wgTransportValue := reflect.NewAt(wgTransportType, unsafe.Pointer(endpointPtr)).Elem()
	ipcConfField := wgTransportValue.FieldByName("ipcConf")
	if !ipcConfField.IsValid() {
		return nil, fmt.Errorf("field 'ipcConf' not found")
	}
	ipcConfPtr := unsafe.Pointer(ipcConfField.UnsafeAddr())

	ipcConf := *(*string)(ipcConfPtr)
	if options.JunkPacketCount > 0 {
		ipcConf += "\njc=" + F.ToString(options.JunkPacketCount)
	}
	if options.JunkPacketMinSize > 0 {
		ipcConf += "\njmin=" + F.ToString(options.JunkPacketMinSize)
	}
	if options.JunkPacketMaxSize > 0 {
		ipcConf += "\njmax=" + F.ToString(options.JunkPacketMaxSize)
	}
	if options.InitPacketJunkSize > 0 {
		ipcConf += "\ns1=" + F.ToString(options.InitPacketJunkSize)
	}
	if options.ResponsePacketJunkSize > 0 {
		ipcConf += "\ns2=" + F.ToString(options.ResponsePacketJunkSize)
	}
	if options.InitPacketMagicHeader > 0 {
		ipcConf += "\nh1=" + F.ToString(options.InitPacketMagicHeader)
	}
	if options.ResponsePacketMagicHeader > 0 {
		ipcConf += "\nh2=" + F.ToString(options.ResponsePacketMagicHeader)
	}
	if options.UnderloadPacketMagicHeader > 0 {
		ipcConf += "\nh3=" + F.ToString(options.UnderloadPacketMagicHeader)
	}
	if options.TransportPacketMagicHeader > 0 {
		ipcConf += "\nh4=" + F.ToString(options.TransportPacketMagicHeader)
	}
	*(*string)(ipcConfPtr) = ipcConf

	return &Endpoint{Endpoint: wg}, nil
}
