.PHONY: test

test:
	go test -v ./...
	# hysteria2x, the direct exit, and the e2e tests that drive QUIC, only build with the tag
	go test -v -tags with_quic ./protocol/ ./protocol/hysteria2x/ ./protocol/unbounded/... ./node/ ./cmd/ ./tracker/clientcontext/ ./peerrules/
	go test -v -tags with_quic -run 'TestHysteria2X|TestDirectExit|TestOutline|TestHedge' ./test/e2e/
