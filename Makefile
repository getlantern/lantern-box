.PHONY: test

test:
	go test -v ./...
	# hysteria2x only exists in QUIC builds, so its e2e needs the tag
	go test -v -tags with_quic -run TestHysteria2X ./test/e2e/
