# lantern-box

Lantern Box is the Lantern fork of [sing-box](https://github.com/SagerNet/sing-box), aka the Universal Proxy Platform. Lantern Box adds additional protocols, such as [Samizdat](https://github.com/getlantern/samizdat), [WATER](https://arxiv.org/html/2312.00163v2), the [proxyless protocol from the Outline SDK](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) that uses things like TLS record fragmentation, TCP packet reordering, and DNS over HTTPS/TLS, etc to bypass DNS and SNI-based blocked, [application layer Geneva](https://www.usenix.org/system/files/sec22-harrity.pdf), [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/), etc.

The goal of Lantern Box is simply to be as useful as possible to the censorship circumvention community and to sing-box in particular. Developers and infrastructure operators are encouraged to run Lantern Box servers and to distribute them to users, and we will continually contribute changes upstream whenever possible.

# Running Lantern Box

You can build and run Lantern Box with:

```bash
cd cmd
make
cd -
./cmd/lantern-box run --config config.json
```

## Samizdat

[Samizdat](https://github.com/getlantern/samizdat) is a censorship circumvention protocol designed to defeat the full spectrum of modern DPI techniques, including those deployed by Russian TSPU infrastructure. It makes proxy traffic indistinguishable from a browser visiting a real website over HTTP/2 by using:

- **Single TLS 1.3 layer** with a Chrome fingerprint via uTLS (no TLS-over-TLS)
- **HTTP/2 CONNECT** tunneling with multiplexed streams on one TCP connection
- **PSK-based authentication** embedded in the TLS SessionID field
- **TCP-level masquerade** to a real domain for active probe resistance
- **Geneva-inspired TCP fragmentation** of the ClientHello at the SNI boundary
- **Traffic shaping** with Chrome-profile padding and timing jitter

Samizdat is implemented as a standalone library ([`samizdat`](https://github.com/getlantern/samizdat)) with zero sing-box dependencies. Lantern Box provides thin adapter code to register it as a sing-box inbound and outbound. See the [samizdat README](https://github.com/getlantern/samizdat/blob/main/README.md) for full protocol details, threat model, and usage.

### Example Configuration

**Client (outbound):**

```json
{
  "outbounds": [
    {
      "type": "samizdat",
      "tag": "samizdat-out",
      "server": "proxy.example.com",
      "server_port": 443,
      "public_key": "ab12cd34ef56...",
      "short_id": "0123456789abcdef",
      "server_name": "ok.ru",
      "fingerprint": "chrome",
      "padding": true,
      "jitter": true,
      "tcp_fragmentation": true,
      "record_fragmentation": true
    }
  ]
}
```

**Server (inbound):**

```json
{
  "inbounds": [
    {
      "type": "samizdat",
      "tag": "samizdat-in",
      "listen": "::",
      "listen_port": 443,
      "private_key": "fedcba9876543210...",
      "short_ids": ["0123456789abcdef"],
      "cert_path": "/etc/ssl/certs/cert.pem",
      "key_path": "/etc/ssl/private/key.pem",
      "masquerade_domain": "ok.ru",
      "max_concurrent_streams": 250
    }
  ]
}
```

Generate keys with the standalone tool:

```bash
go run github.com/getlantern/samizdat/cmd/samizdat-server --genkeys
```
