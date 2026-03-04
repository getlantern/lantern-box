# lantern-box

Lantern Box is the Lantern fork of [sing-box](https://github.com/SagerNet/sing-box) -- the universal proxy platform -- with extra protocols built for places where the internet comes with walls. It adds [ALGeneva](https://www.usenix.org/system/files/sec22-harrity.pdf), [WATER](https://arxiv.org/html/2312.00163v2), [Samizdat](https://github.com/getlantern/samizdat), [Outline SDK smart dialer](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart), and [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/) to the sing-box ecosystem.

The goal is to be as useful as possible to the censorship circumvention community. Operators are encouraged to run servers and hand configs to users. We contribute changes upstream whenever we can.

---

## Quick Start

Build:

```bash
cd cmd
make
cd -
```

Run:

```bash
./cmd/lantern-box run --config config.json
```

That's it. The rest of this document is about what goes inside `config.json`.

---

## Protocol Guide

Pick the protocol that fits your threat model.

| Protocol | Threat Model | Keys/Certs? | Server Needed? |
|---|---|---|---|
| **Samizdat** | Full DPI resistance (Russia-grade TSPU) | X25519 + TLS cert | Yes |
| **WATER** | Pluggable transport (swap WASM modules) | WASM hash | Yes |
| **Outline SDK** | DNS/SNI blocking (smart dialer) | No | No |
| **AmneziaWG** | WireGuard protocol fingerprinting | WireGuard keys | Yes |
| **ALGeneva** | HTTP-level DPI (header inspection) | No | Yes |

---

### Samizdat

[Samizdat](https://github.com/getlantern/samizdat) is built to defeat the full spectrum of modern DPI, including Russian TSPU infrastructure. It makes proxy traffic look like a browser visiting a real website over HTTP/2:

- **Single TLS 1.3 layer** with a Chrome fingerprint via uTLS (no TLS-over-TLS)
- **HTTP/2 CONNECT** tunneling with multiplexed streams on one TCP connection
- **PSK-based authentication** embedded in the TLS SessionID field
- **TCP-level masquerade** to a real domain for active probe resistance
- **Geneva-inspired TCP fragmentation** of the ClientHello at the SNI boundary
- **Traffic shaping** with Chrome-profile padding and timing jitter

**When to use it:** The censor is doing deep packet inspection, active probing, and traffic analysis. You need the heavy artillery.

See the [samizdat README](https://github.com/getlantern/samizdat/blob/main/README.md) for the full protocol design.

#### Credential generation

You need an X25519 keypair, a short ID, and a TLS certificate.

**Option A -- samizdat tool:**

```bash
go run github.com/getlantern/samizdat/cmd/samizdat-server --genkeys
```

**Option B -- openssl (for CI or scripting):**

```bash
# X25519 keypair
openssl genpkey -algorithm X25519 -out priv.pem
PRIVATE_KEY=$(openssl pkey -in priv.pem -outform DER | tail -c 32 | xxd -p | tr -d '\n')
PUBLIC_KEY=$(openssl pkey -in priv.pem -pubout -outform DER | tail -c 32 | xxd -p | tr -d '\n')

# Random short ID
SHORT_ID=$(openssl rand -hex 8)

# Self-signed TLS certificate
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -keyout key.pem -out cert.pem -days 365 -nodes \
  -subj "/CN=example.com"
```

#### Server config

```json
{
  "log": { "level": "info" },
  "inbounds": [
    {
      "type": "samizdat",
      "tag": "samizdat-in",
      "listen": "::",
      "listen_port": 443,
      "private_key": "PRIVATE_KEY_HEX",
      "short_ids": ["SHORT_ID_HEX"],
      "cert_path": "/etc/ssl/certs/cert.pem",
      "key_path": "/etc/ssl/private/key.pem",
      "masquerade_domain": "ok.ru"
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ]
}
```

#### Client config

```json
{
  "inbounds": [
    {
      "type": "mixed",
      "tag": "mixed-in",
      "listen": "127.0.0.1",
      "listen_port": 1080
    }
  ],
  "outbounds": [
    {
      "type": "samizdat",
      "tag": "samizdat-out",
      "server": "YOUR_SERVER_IP",
      "server_port": 443,
      "public_key": "PUBLIC_KEY_HEX",
      "short_id": "SHORT_ID_HEX",
      "server_name": "ok.ru"
    }
  ]
}
```

#### All client options

| Field | Type | Default | Description |
|---|---|---|---|
| `public_key` | string | *required* | Server X25519 public key (64 hex chars) |
| `short_id` | string | *required* | Pre-shared 8-byte ID (16 hex chars) |
| `server_name` | string | | Cover site SNI (e.g. `"ok.ru"`) |
| `fingerprint` | string | `"chrome"` | TLS fingerprint: `"chrome"`, `"firefox"`, `"safari"` |
| `disable_padding` | bool | `false` | Disable H2 DATA frame padding |
| `disable_jitter` | bool | `false` | Disable timing jitter |
| `max_jitter_ms` | int | `30` | Maximum jitter in milliseconds |
| `padding_profile` | string | `"chrome"` | Padding profile: `"chrome"`, `"firefox"` |
| `disable_tcp_fragmentation` | bool | `false` | Disable ClientHello fragmentation |
| `disable_record_fragmentation` | bool | `false` | Disable TLS record fragmentation |
| `max_streams_per_conn` | int | `100` | Max H2 streams per TCP connection |
| `idle_timeout` | string | `"5m"` | Close idle connections after this duration |
| `connect_timeout` | string | `"15s"` | TCP+TLS connect timeout |
| `data_threshold` | int | `14000` | Bytes before aggressive padding kicks in |

#### All server options

| Field | Type | Default | Description |
|---|---|---|---|
| `private_key` | string | *required* | Server X25519 private key (64 hex chars) |
| `short_ids` | string[] | *required* | Allowed client short IDs |
| `cert_path` | string | | Path to TLS certificate PEM file |
| `key_path` | string | | Path to TLS key PEM file |
| `cert_pem` | string | | Inline TLS certificate PEM |
| `key_pem` | string | | Inline TLS key PEM |
| `masquerade_domain` | string | | Domain to masquerade as |
| `masquerade_addr` | string | | IP:port override for masquerade |
| `masquerade_idle_timeout` | string | `"5m"` | Masquerade connection idle timeout |
| `masquerade_max_duration` | string | `"10m"` | Max masquerade connection duration |
| `max_concurrent_streams` | int | `250` | Max H2 streams per connection |

---

### WATER

[WATER](https://arxiv.org/html/2312.00163v2) (WebAssembly Transport Executables Runtime) lets you swap transport logic at runtime by loading WASM modules. Both sides download the same WASM binary; the module handles how bytes move over the wire.

**When to use it:** You want pluggable transports without recompiling. New evasion logic ships as a `.wasm` file.

#### Credentials

You need a WASM module and its SHA-256 hash. The `plain.wasm` test module is bundled in the WATER Go module:

```bash
# Download the WATER module
go mod download github.com/refraction-networking/water@v0.7.1-alpha

# Find plain.wasm in the module cache
WASM_PATH="$(go env GOPATH)/pkg/mod/github.com/refraction-networking/water@v0.7.1-alpha/transport/v1/testdata/plain.wasm"

# Compute the SHA-256 hash
shasum -a 256 "$WASM_PATH"
```

Host the `.wasm` file on an HTTP server both sides can reach (a simple `python3 -m http.server 8888` works).

#### Server config

```json
{
  "log": { "level": "info" },
  "inbounds": [
    {
      "type": "water",
      "tag": "water-in",
      "listen": "::",
      "listen_port": 9003,
      "transport": "plain",
      "hashsum": "WASM_SHA256_HASH",
      "wasm_available_at": ["http://127.0.0.1:8888/plain.wasm"],
      "config": {}
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ]
}
```

#### Client config

```json
{
  "inbounds": [
    {
      "type": "mixed",
      "tag": "mixed-in",
      "listen": "127.0.0.1",
      "listen_port": 1080
    }
  ],
  "outbounds": [
    {
      "type": "water",
      "tag": "water-out",
      "server": "YOUR_SERVER_IP",
      "server_port": 9003,
      "transport": "plain",
      "hashsum": "WASM_SHA256_HASH",
      "wasm_available_at": ["http://YOUR_SERVER_IP:8888/plain.wasm"],
      "download_timeout": "60s",
      "water_dir": "/tmp/water",
      "config": {}
    }
  ]
}
```

> **Gotcha:** The `"config"` field is optional. If omitted, WATER will still start, but it will not inject `remote_addr`/`remote_port` into the module config. Include `"config": {}` when your WASM module expects those values.

#### All client options

| Field | Type | Description |
|---|---|---|
| `transport` | string | Identifier for WASM logs |
| `hashsum` | string | SHA-256 of the WASM file (integrity check) |
| `wasm_available_at` | string[] | URLs to download the WASM module |
| `download_timeout` | string | Required. Per-URL download timeout (e.g. `"60s"`). No default is applied; must be a valid Go duration string. |
| `water_dir` | string | Required. Local directory for WATER files; must not be empty. |
| `config` | object | Config passed to the WASM module (can be `{}`) |
| `skip_handshake` | bool | Set `true` if the WASM module handles its own handshake |

---

### Outline SDK (Smart Dialer)

The [Outline SDK smart dialer](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) is an outbound-only protocol that automatically tries DNS and TLS evasion strategies to reach blocked sites. It cycles through DNS resolvers (system, DoH, DoT, UDP, TCP) and TLS tricks (record fragmentation, packet reordering) until something works.

**When to use it:** DNS or SNI-based blocking, no server infrastructure available. The client figures out how to get through on its own.

**No server config needed.** This is a client-side dialer.

#### Client config

```json
{
  "inbounds": [
    {
      "type": "mixed",
      "tag": "mixed-in",
      "listen": "127.0.0.1",
      "listen_port": 1080
    }
  ],
  "outbounds": [
    {
      "type": "outline",
      "tag": "outline-out",
      "server": "blocked-site.example.com",
      "server_port": 443,
      "dns": [
        { "system": {} },
        { "https": { "name": "cloudflare-dns.com" } },
        { "tls": { "name": "dns.google", "address": "8.8.8.8:853" } },
        { "udp": { "address": "8.8.8.8:53" } },
        { "tcp": { "address": "1.1.1.1:53" } }
      ],
      "tls": ["fragmentation", "reordering"],
      "test_timeout": "10s",
      "domains": ["blocked-site.example.com"]
    }
  ]
}
```

#### DNS resolver types

| Type | Config | Description |
|---|---|---|
| `system` | `{}` | Use OS resolver |
| `https` | `{ "name": "cloudflare-dns.com" }` | DNS-over-HTTPS |
| `tls` | `{ "name": "dns.google", "address": "8.8.8.8:853" }` | DNS-over-TLS |
| `udp` | `{ "address": "8.8.8.8:53" }` | Plain UDP DNS |
| `tcp` | `{ "address": "1.1.1.1:53" }` | Plain TCP DNS |

---

### AmneziaWG

[AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/) extends WireGuard with junk packet injection and magic header rewriting to defeat protocol fingerprinting. It's configured as a sing-box endpoint (not an inbound/outbound) with extra parameters on top of a standard WireGuard config.

**When to use it:** WireGuard is blocked by protocol fingerprinting. You need WireGuard semantics but with an unrecognizable wire format.

#### Endpoint config

Add the Amnezia parameters alongside your WireGuard endpoint config:

```json
{
  "endpoints": [
    {
      "type": "amneziawg",
      "tag": "awg-ep",
      "address": ["10.0.0.2/32"],
      "private_key": "WIREGUARD_PRIVATE_KEY",
      "peers": [
        {
          "public_key": "WIREGUARD_SERVER_PUBLIC_KEY",
          "endpoint": "YOUR_SERVER_IP:51820",
          "allowed_ips": ["0.0.0.0/0"]
        }
      ],
      "junk_packet_count": 4,
      "junk_packet_min_size": 40,
      "junk_packet_max_size": 70,
      "init_packet_junk_size": 10,
      "response_packet_junk_size": 10,
      "init_packet_magic_header": 1,
      "response_packet_magic_header": 2,
      "underload_packet_magic_header": 3,
      "transport_packet_magic_header": 4
    }
  ]
}
```

#### Amnezia parameters

| Field | JSON key | Description |
|---|---|---|
| Jc | `junk_packet_count` | Number of junk packets sent before session init |
| Jmin | `junk_packet_min_size` | Minimum junk packet size in bytes |
| Jmax | `junk_packet_max_size` | Maximum junk packet size in bytes |
| S1 | `init_packet_junk_size` | Junk bytes prepended to init handshake |
| S2 | `response_packet_junk_size` | Junk bytes prepended to response handshake |
| H1 | `init_packet_magic_header` | Magic header for init packets |
| H2 | `response_packet_magic_header` | Magic header for response packets |
| H3 | `underload_packet_magic_header` | Magic header for underload packets |
| H4 | `transport_packet_magic_header` | Magic header for transport packets |

Both client and server must use identical Amnezia parameters. See the [AmneziaWG docs](https://docs.amnezia.org/documentation/amnezia-wg/) for parameter tuning guidance.

---

### ALGeneva

Application-layer [Geneva](https://www.usenix.org/system/files/sec22-harrity.pdf) -- mutates HTTP headers on the fly to evade DPI that inspects header fields. No keys, no certs, no TLS. Just a strategy string that describes how to mangle traffic.

**When to use it:** The censor is doing simple HTTP header inspection. You want the fastest possible setup with zero credential management.

**Strategy syntax:** `[trigger]-action-|` where the trigger matches a header field and the action transforms it. Example: `[HTTP:host:*]-changecase{lower}-|` lowercases the Host header.

#### Server config

```json
{
  "log": { "level": "info" },
  "inbounds": [
    {
      "type": "algeneva",
      "tag": "algeneva-in",
      "listen": "::",
      "listen_port": 9001
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ]
}
```

The server doesn't need a strategy -- it just accepts connections and forwards them.

#### Client config

```json
{
  "inbounds": [
    {
      "type": "mixed",
      "tag": "mixed-in",
      "listen": "127.0.0.1",
      "listen_port": 1080
    }
  ],
  "outbounds": [
    {
      "type": "algeneva",
      "tag": "algeneva-out",
      "server": "YOUR_SERVER_IP",
      "server_port": 9001,
      "strategy": "[HTTP:host:*]-changecase{lower}-|"
    }
  ]
}
```

Point your browser's SOCKS5 proxy at `127.0.0.1:1080` and you're done.

---

## Running Multiple Protocols

A single lantern-box instance can serve multiple protocols. Just put them all in one config:

```json
{
  "log": { "level": "info" },
  "inbounds": [
    {
      "type": "algeneva",
      "tag": "algeneva-in",
      "listen": "::",
      "listen_port": 9001
    },
    {
      "type": "samizdat",
      "tag": "samizdat-in",
      "listen": "::",
      "listen_port": 9002,
      "private_key": "PRIVATE_KEY_HEX",
      "short_ids": ["SHORT_ID_HEX"],
      "cert_path": "/etc/ssl/certs/cert.pem",
      "key_path": "/etc/ssl/private/key.pem",
      "masquerade_domain": "ok.ru"
    },
    {
      "type": "water",
      "tag": "water-in",
      "listen": "::",
      "listen_port": 9003,
      "transport": "plain",
      "hashsum": "WASM_SHA256_HASH",
      "wasm_available_at": ["http://127.0.0.1:8888/plain.wasm"],
      "config": {}
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ]
}
```

---

## E2E Testing

The E2E test suite spins up a DigitalOcean droplet, deploys lantern-box, and runs traffic through ALGeneva, Samizdat, and WATER.

**Trigger it manually:**

```bash
gh workflow run e2e.yaml
```

It also runs automatically on pull requests to `main` that modify non-documentation files (docs-only changes are ignored).

**What it tests:** Each protocol gets a server instance on the droplet and a client instance on the CI runner. The test curls `http://example.com` through each proxy and checks for a valid response.

**Required secrets:** `DO_API_TOKEN` (DigitalOcean), `CI_PRIVATE_REPOS_GH_TOKEN` (private Go modules).

---

## Contributing

PRs welcome. The upstream goal means we prefer changes that are general enough to contribute back to sing-box when possible.

- Protocol adapters live in `protocol/`
- Option structs live in `option/`
- Build tags: `with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale`

Run tests with:

```bash
go test -tags "with_gvisor,with_quic,with_dhcp,with_wireguard,with_utls,with_acme,with_clash_api,with_tailscale" ./...
```
