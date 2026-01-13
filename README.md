# lantern-box

Lantern Box is the Lantern fork of [sing-box](https://github.com/SagerNet/sing-box), aka the Universal Proxy Platform. Lantern Box adds additional protocols, such as [WATER](https://arxiv.org/html/2312.00163v2), the [proxyless protocol from the Outline SDK](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) that uses things like TLS record fragmentation, TCP packet reordering, and DNS over HTTPS/TLS, etc to bypass DNS and SNI-based blocked, [application layer Geneva](https://www.usenix.org/system/files/sec22-harrity.pdf), [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/), etc.

Lantern Box is intended to be as useful as possible to the censorship circumvention community and to sing-box in particular. Developers and infrastructure operators are encouraged to run Lantern Box servers and to distribute them to users, and we will continually contribute changes upstream whenever possible.

# Running Lantern Box

You can build and run Lantern Box with:

```bash
cd cmd
make
cd -
./cmd/lantern-box run --config config.json
```
