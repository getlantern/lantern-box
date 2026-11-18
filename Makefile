.PHONY: test

test:
	go test -v ./...
	# hysteria2x and the direct exit only exist in QUIC builds, so their tests need the tag
	go test -v -tags with_quic ./protocol/ ./protocol/hysteria2x/ ./protocol/unbounded/... ./node/ ./cmd/ ./tracker/clientcontext/ ./peerrules/
	go test -v -tags with_quic -run 'TestHysteria2X|TestDirectExit' ./test/e2e/
