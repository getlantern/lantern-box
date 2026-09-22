.PHONY: test

test:
	go test -v ./...
	# hysteria2x only exists in QUIC builds, so its tests need the tag
	go test -v -tags with_quic ./protocol/hysteria2x/
	go test -v -tags with_quic -run TestHysteria2X ./test/e2e/
