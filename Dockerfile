FROM --platform=$BUILDPLATFORM golang:1.24-bullseye as builder
ARG TARGETOS TARGETARCH
ARG GOPROXY=""
ARG VERSION=""
ARG COMMIT=""

RUN set -ex \
    && apt-get update \
    && if [ "$TARGETARCH" = "arm64" ]; then \
           apt-get install -y gcc-aarch64-linux-gnu g++-aarch64-linux-gnu qemu-user; \
       else \
           apt-get install -y gcc g++; \
       fi \
    && rm -rf /var/lib/apt/lists/*

WORKDIR $GOPATH/src/getlantern/lantern-box/
COPY . .

ENV CGO_ENABLED=1
ENV GOOS=$TARGETOS
ENV GOARCH=$TARGETARCH

RUN set -ex \
    && if [ "$TARGETARCH" = "arm64" ]; then \
           export CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++; \
       else \
           export CC=gcc CXX=g++; \
       fi && \
       echo "Building for $GOOS/$GOARCH using CC=$CC CXX=$CXX" && \
       go build -v -tags "with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api" \
       -ldflags="-X main.version=${VERSION} -X main.commit=${COMMIT}" \
       -o /usr/local/bin/lantern-box ./cmd

FROM debian:bullseye-slim
RUN set -ex \
    && apt-get update \
    && apt-get install -y ca-certificates tzdata nftables wireguard-tools \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/lantern-box /usr/local/bin/lantern-box

ENTRYPOINT ["/usr/local/bin/lantern-box", "run", "--config", "/config.json"]
