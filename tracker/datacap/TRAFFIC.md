# Coarse traffic reporting

Datacap tracking can optionally classify traffic locally into `lantern`, `probe`,
`other`, and `unknown`, using the requested hostname available at routing time.
Reports contain byte counters and bidirectional TCP connection counts only; no
destination hostnames, IPs, URLs or content are added to the reporting payload.
This collection does not gate connections or change throttling.

Enable `LANTERN_TRAFFIC_CATEGORIES=1` on a proxy already using `--datacap-url`.
The default is off. Deploy a sidecar supporting the additive `trafficUsage`
JSON field before enabling it: older sidecars reject unknown JSON fields.
The companion lantern-cloud implementation forwards these counters and can
retain them under `datacap_usage_history_enabled`, which also defaults off.

`LANTERN_TRAFFIC_SERVICE_HOSTS` and `LANTERN_TRAFFIC_PROBE_HOSTS` are optional
comma-separated additions to the built-in exact-match host lists. Embedders
can configure the equivalent `TrafficCategories`, `LanternHosts` and
`ProbeHosts` options. Case and a final dot are normalized. No suffix matching,
DNS resolution or HTTPS inspection is performed.

API and callback traffic sharing a Lantern hostname is classified as `lantern`.
`probe` means a known probe hostname, not a verified probe request: that host
may also serve other content. Unlisted service hosts may appear as `other`,
which also includes automated traffic and is not evidence of humanity.
IP-only destinations are unknown. UDP associations can contain several
destinations and report unknown bytes, without connection counts.

A TCP connection contributes a connection count only once after bytes have
been read and written, on a report acknowledged by the sidecar. Failed reports
retain the pending count for retry. An acknowledgement lost after ingestion
can still cause duplication because the existing datacap protocol reports
deltas without stable report IDs. Bidirectional bytes do not establish
application success. Connections without client-context metadata and Pro
clients retain the existing datacap exclusion behavior.

Verify with `go test -race -timeout=180s ./tracker/datacap`.
