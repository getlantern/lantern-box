.PHONY: test

test:
	go test -v ./...
	# hysteria2x, and the e2e tests that drive QUIC, only build with the tag
	go test -v -tags with_quic ./protocol/ ./protocol/hysteria2x/
	go test -v -tags with_quic -run 'TestHysteria2X|TestOutlineQUICPrelude|TestHedge' ./test/e2e/
