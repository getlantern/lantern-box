TAGS = with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale

build:
	go build -v -tags "$(TAGS)" -o lantern-box .
run:
	go run -tags=$(TAGS) . run --config $(config)
