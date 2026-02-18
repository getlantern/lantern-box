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
