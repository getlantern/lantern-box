build:
	go build -v -tags \
		"with_gvisor,with_dhcp,with_wireguard,with_reality_server,with_clash_api,with_quic,with_utls,with_acme" \
		-o lantern-box .
run:
	go run -tags=with_gvisor,with_dhcp,with_wireguard,with_reality_server,with_clash_api,with_quic,with_utls,with_acme . run --config $(config)
