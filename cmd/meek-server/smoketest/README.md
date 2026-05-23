# meek-server smoke tests

End-to-end validation that a deployed meek-server behind a CDN actually
shuttles bytes from a fronted HTTPS client all the way to the public
internet via the upstream proxy.

## Reference deployment (verified 2026-05-23)

```
                            outer SNI = a248.e.akamai.net  (or any Akamai-fronted host)
                            inner Host = meek.dsa.akamai.getiantem.org
client ──────HTTPS─────► Akamai DSA ──────HTTP/HTTPS─────► Linode :443
                                                                │
                                                            Caddy (TLS terminator,
                                                            LE cert for meek.getiantem.org)
                                                                │ HTTP
                                                                ▼
                                                            meek-server :8080
                                                            (-upstream 127.0.0.1:1080)
                                                                │ TCP
                                                                ▼
                                                            microsocks :1080
                                                            (SOCKS5, direct outbound)
                                                                │
                                                                ▼
                                                            public internet
```

Akamai property: `meek.dsa.akamai.getiantem.org`, edge hostname
`meek.dsa.akamai.getiantem.org.edgesuite.net` (Shared Cert / Standard
TLS — auto-covered by the edgesuite wildcard at the SNI layer used by
fronted clients).

Cloudflare DNS on `getiantem.org`:
- `meek.dsa.akamai` CNAME → `meek.dsa.akamai.getiantem.org.edgesuite.net` (DNS-only)
- `meek` A → 139.162.181.47 (origin direct, for Caddy's LE challenge and
  for Akamai's origin connection)

## socks5.sh

Sequential SOCKS5 handshake + HTTP `GET /ip` against `httpbin.org:80`
via the proxy. A successful run prints the origin IP httpbin observed
— it should be the Linode's public IP, confirming the request actually
exited the box.

Run from anywhere with network access:

```bash
./socks5.sh
```

Override the front or inner host for a different deployment:

```bash
FRONT_HOST=a248.e.akamai.net \
INNER_HOST=meek.dsa.akamai.getiantem.org \
./socks5.sh
```

### How it works

microsocks requires strict SOCKS5 request-response, so the script
does the dance in three phases through the meek tunnel:

1. **Method-select**: POST 3 bytes (`05 01 00`) → expect `05 00`
2. **CONNECT**: POST `05 01 00 03 <len> httpbin.org <port>` → expect `05 00 00 01 ...`
3. **HTTP**: POST `GET /ip HTTP/1.0\r\n...` → drain the HTTP response

Each phase is one or more `POST /` calls with the same `X-Session-Id`
header so meek-server routes them to the same upstream TCP connection.
Follow-up empty POSTs are used to drain bytes that the upstream wrote
while the script was building the next request.

### What a successful run looks like

```
✅ End-to-end SUCCESS: "origin": "139.162.181.47"
The request traversed: curl → Akamai → Caddy → meek-server → microsocks → httpbin.org
```
