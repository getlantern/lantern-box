# Volunteer Direct Exit (lantern-box) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Unbounded volunteers exit users' traffic themselves over a bare ICE path with Hysteria2. Users' apps take those exits, and everything else falls back to today's path to the egress server.

**Architecture:**
- lantern-box owns the whole direct exit, as it owns Lamplighter nodes:
  - a WebRTC-only registrar in `node/` that registers, rotates and builds an exit box from lantern-cloud's config;
  - an exit server (`protocol/unbounded/exit`) that serves one Hysteria2 session per ICE path and routes its streams into the exit box;
  - a direct-exit client in the `unbounded` outbound.
- broflake's `clientcore` (getlantern/unbounded#524, merged as `60579fa`) supplies the ICE path and the hooks: `ExitOptions`, `OnExitPath` and `TakeExit`.
- radiance only wires these in (a later plan).

**Tech Stack:** Go; sing-box (getlantern/sing-box-minimal v1.14.2-lantern.3); sing-quic hysteria2 v0.7.1-0.20260924092235-6a3a24d65b99; broflake clientcore; pion ICE (inside broflake).

**Spec:** `getlantern/unbounded` `docs/superpowers/specs/2026-11-17-volunteer-direct-exit-design.md`, at `/Users/afisk/go/src/github.com/getlantern/unbounded-direct-exit/docs/superpowers/specs/2026-11-17-volunteer-direct-exit-design.md`.

**Departures from the spec, approved by the user on 2026-11-18:**
- **A separate exit box.** The app volunteer's exit runs in its own sing-box built from the registration's config, with `route.auto_detect_interface` on, as radiance's direct-mode peer box does. The spec said the config would be merged into the VPN box. Keeping the boxes apart keeps the user's own routing out of strangers' exit traffic.
- **No Salamander yet.** Both sides must agree on it per session, and that needs a field in the ICE answer. Until then, the fallback for a country that blocks the pattern is turning `direct_exit` off there.
- **Registration lives in `node/`, as Lamplighter's does.** It is not in radiance, and the API client gets a caller-supplied header hook so the app's admission headers go through.

## Global Constraints

- Never log IP addresses, destinations, ICE credentials, cert pins, session passwords, device IDs or route IDs. Count refusals; never record hosts. A logged error must not wrap a value that can carry any of these.
- Congestion control is BBR only. The server sets `IgnoreClientBandwidth: true` with `ReceiveBPS: 0` and `SendBPS: 0`; the client sets `SendBPS: 0` and `ReceiveBPS: 0`. Brutal must never be negotiated.
- The client's `pre_initial_junk` is always on for a direct exit.
- Session password: 32 random bytes from `crypto/rand`, base64 raw URL encoded. The certificate is self-signed ECDSA P-256, generated at start and kept only in memory. The pin is the standard base64 encoding of SHA-256 over the leaf's PKIX public key, matching sing-box's `certificate_public_key_sha256`.
- The inbound tag for exit traffic is `unbounded-exit`.
- After any `go get`, run `go mod tidy` and commit `go.mod` and `go.sum` together.
- Code that imports sing-quic goes in `//go:build with_quic` files, with a `!with_quic` stub, as `protocol/hysteria2x` does.
- Tests: `go test ./...` and `go test -tags with_quic ./...` must both pass, as must the Makefile's targets.
- Port-mapped direct mode (samizdat) and reverse direct mode are unchanged.
- Implementers and reviewers use the strongest model (opus).

## lantern-cloud contract (implemented in the lantern-cloud plan)

`POST {APIURL}/peer/register` with body `{"mode":"webrtc","serve_pool":<bool>}` and no ports. `serve_pool` is the volunteer's "help strangers too" setting (`ExitConfig.ServePool`, read at each registration; unset means true, as radiance's `servePool` treats it). lantern-cloud admits the request as today: the feature flag, `X-Lantern-App: lantern-box`, or a paired volunteer. It refuses with 403 for a device that isn't volunteering for Unbounded, is blocked, or is in a censored or unknown country. A 200 response:

```json
{"server_config": "<pool policy JSON>", "personal_server_config": "<personal policy JSON>", "rotation_interval_seconds": 3600}
```

The two configs are one per kind of session, as broflake reports it (getlantern/unbounded#525): a **personal** session answered a genesis sent to the volunteer's rooms alone, so it is a friend's; every other session is a **pool** session, a stranger's.

- `server_config`, the pool policy, is present only when `serve_pool` is true and lantern-cloud lets this device serve the pool. It carries the residential allowlist (`applyPeerAllowlist`).
- `personal_server_config`, the personal policy, is present only when the device has helper pairings. It has no allowlist (`applyPersonalExit`, lantern-cloud 598f6adc0).
- At least one is present. A response with one config absent clears that kind at once, ending its sessions; a response with neither is a failure, and RunExit keeps both current boxes until they expire, as for any failed registration.

RunExit builds a box from each config present and sets the exit's router for that kind (`exit.Pool`, `exit.Personal`). A session routes only through its own kind's router and never falls back to the other's, and a kind whose config is absent is cleared, ending its sessions. The kinds fail and expire apart: a config that fails RunExit's checks leaves that kind's current box serving until its own registration runs out. A kind whose config is unchanged keeps its box, its registration extended, so a failing config for one kind, which makes RunExit re-register at every retry, never resets the other kind's streams. On Linux, a routing mark the process can't set (no CAP_NET_ADMIN) serves no box, rather than one whose every dial fails. A box is served only once its four abuse rule sets have loaded: sing-box starts with a remote rule set it couldn't download empty, so RunExit refuses such a box (the kind's current box, if any, serves on, and RunExit retries on its back-off), and a Lamplighter node's volunteer relays only until sing-box's own retries load them. The check is continuous, and made at the point of use: a later download or reload can replace a set's rules with none, and sing-box publishes them before telling anyone, so the exit checks the sets of the kind's box before routing each stream or flow and before offering or minting each session, refusing them while any set is empty and accepting them again once all hold rules. RunExit also checks before publishing a box (one that never loaded isn't published, and the kind's current box serves on) and before renewing an unchanged box (one whose sets are empty isn't extended). A set counts as loaded only while it holds a rule, local and inline ones included. When `serve_pool` changes, the embedder sends on `ExitConfig.Reregister` (as radiance re-registers its peer route, f25ca27), and the next registration's configs replace the current ones.

A Lamplighter node's box carries one policy, which lantern-cloud picks by the node registration's `serve_pool`. Nodes register with `serve_pool` true, so a node's volunteer sets the pool router alone, and its personal sessions, if any, are offered no direct exit and relay to the egress as before.

Each config has a `direct` outbound, `route.rule_set` with the four abuse tags, `dns`, and `route.rules` with the same reject rules as a direct-mode registration (allowlist for the pool). Every `mode:"webrtc"` config, pool or personal, must have, ahead of any rule that can route `unbounded-exit` traffic to an outbound:

1. an unconditional `resolve` action rule (sing-box's `ip_cidr` and `ip_is_private` match a domain destination only after one);
2. an unconditional `{"ip_is_private": true, "action": "reject"}`;
3. the abuse rule-set rejects and the static IP and port rejects.

The config has exactly one `resolve` rule, the unconditional one above: a later resolve (one with `disable_cache`, say) replaces the destination's addresses after the rejects checked the first lookup. RunExit refuses a config with any other resolve, conditional or inside a logical rule.

The abuse rule-set rejects must also come before any `sniff` rule. A sniffed HTTP Host or TLS SNI replaces the domain the geosite rule sets match, so a client can name an allowed host in its request and reach a listed one. lantern-cloud's bandit-probe injection (`injectBanditProbe`) prepends a port-80 sniff ahead of them when a config has endpoints, so the `mode:"webrtc"` builder must not inject one ahead of the rejects. Today's samizdat pool configs have the same hole and should be fixed there too. lantern-box's check does not model sniffing, so it does not catch this.

Every rule that routes to `direct` must set `udp_connect: true` (a Hysteria2 UDP session names a destination per packet, and the rules see only the first): the terminal route is an explicit final rule rather than `route.final`, which can't set it, and for the pool it is the allowlist's route rule ahead of its terminal reject. lantern-cloud's `applyPeerAllowlist` and `applyPersonalExit` already build this shape for samizdat peers. The config has no `experimental` (no debug listener), no inbounds, no endpoints and no services. RunExit refuses a config, of either kind, without the resolve and the `ip_is_private` reject after it, and drops any inbounds, endpoints, services, `experimental` and `ntp` regardless. There is no route row, verify, heartbeat or deregister.

The client treats a registration as valid for 2 × the rotation interval (`rotation_interval_seconds`, capped at 24h; 0 means an hour), so a device lantern-cloud starts refusing keeps exiting traffic for up to that long after its last successful registration.

lantern-cloud also adds, through config-new:
- `direct_exit` (bool) to the volunteer's features;
- `direct_exit: {"server_name": "<sni>"}` to the user's `unbounded` outbound options;
- `"direct_exit": true` to a Lamplighter node's volunteer service options when the flag is on. The node (and `lantern-box run`) turns it off unless the box's config passes the abuse check and has the resolve and `ip_is_private` reject above; the pool peer config the volunteer service rides on already does.

## File Structure

| File | Responsibility |
|---|---|
| `go.mod`, `go.sum` | broflake bumped to `60579fa` |
| `peerrules/peerrules.go`, `peerrules/peerrules_test.go` | Moved from radiance `peer/validate.go`: the abuse-rule check, exported |
| `protocol/internal/quicjunk/junk.go`, `junk_test.go` | The pre-Initial junk datagram, shared by hysteria2x and the direct-exit client |
| `protocol/hysteria2x/outbound.go` | Uses `quicjunk` |
| `protocol/unbounded/exit/cert.go` | In-memory certificate and pin |
| `protocol/unbounded/exit/exit.go` | `Exit`: readiness, credentials, `Serve`, the clientcore hooks (`with_quic`) |
| `protocol/unbounded/exit/handler.go` | Routes Hysteria2 streams and flows into the current router as `unbounded-exit` (`with_quic`) |
| `protocol/unbounded/exit/stub.go` | `!with_quic` stub |
| `protocol/unbounded/exit/metrics.go` | otel counters |
| `protocol/unbounded/exit/*_test.go` | Unit and loopback tests |
| `node/api.go` | Header hook, `registerWebRTC` |
| `node/exit.go`, `node/exit_test.go` | `RunExit`: WebRTC-only registration, rotation, exit box |
| `option/unbounded.go` | `UnboundedVolunteerOptions.DirectExit`, `UnboundedOutboundOptions.DirectExit` |
| `protocol/unbounded/volunteer.go` | Lamplighter nodes serve direct exits from their own router |
| `node/node.go` | `launch` strips `direct_exit` when the rules fail `peerrules` |
| `protocol/unbounded/exitpaths.go`, `exitpaths_stub.go`, `exitpaths_test.go` | The user's side: Hysteria2 clients over `ExitPath`s, back-off |
| `protocol/unbounded/outbound.go` | Dials through a live direct exit first |
| `test/e2e/direct_exit_test.go`, `test/e2e/testdata/direct_exit_server.json` | In-process end to end with the real freddie |

---

### Task 1: Bump broflake to the merged direct-exit commit

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `UBClientcore.ExitOptions{Ready func() bool; Params func() (certPin, password string, ok bool); Serve func(ctx context.Context, conn net.PacketConn, password string); OnSession func(directExit bool)}`, `UBClientcore.ExitPath{Conn net.PacketConn; CertPin, Password string; Relayed bool}`, `WebRTCOptions.Exit`, `WebRTCOptions.OnExitPath func(ExitPath)`, `WebRTCOptions.TakeExit func() bool`, `ConsumerSlot.DirectExit bool`.

- [ ] **Step 1: Bump and tidy**

```bash
go get github.com/getlantern/broflake@60579fa9b3a0ba98288bc46f67fe95ae46d4d6f8
go mod tidy
git diff go.mod
```
Expected: only the broflake line, plus whatever tidy needs for it (pion/ice/v4 and pion/logging may become direct in broflake but stay indirect here).

- [ ] **Step 2: Build and test**

Run: `go build ./... && go test ./protocol/unbounded/... ./node/... && go test -tags with_quic ./protocol/... `
Expected: PASS. broflake's exported API only gained fields and functions, so no lantern-box code changes.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "Bump broflake to the direct exit hooks (getlantern/unbounded#524)"
```

---

### Task 2: Move the abuse-rule check into `peerrules`

**Files:**
- Create: `peerrules/peerrules.go`, `peerrules/peerrules_test.go`

**Interfaces:**
- Produces:
  - `func ValidateAbuseRules(optionsJSON string) error`
  - `func ValidateAbuseRulesOptions(options option.Options) error`

The code is radiance `origin/main:peer/validate.go` (fdd4002), unchanged except:
- package `peerrules`;
- `validateAbuseRules` becomes `ValidateAbuseRules`, using `box.BaseContext()` from `github.com/getlantern/lantern-box` (radiance used `box.Context(context.Background())`; check `context.go:16-55` and use whichever registers lantern-box's option types — `BaseContext` does);
- the body after decoding moves into `ValidateAbuseRulesOptions`, which `ValidateAbuseRules` calls.

- [ ] **Step 1: Copy the test**

```bash
git -C ../radiance fetch -q origin
git -C ../radiance show origin/main:peer/validate_test.go > peerrules/peerrules_test.go
git -C ../radiance show origin/main:peer/validate.go > peerrules/peerrules.go
```
In the test, change `package peer` to `package peerrules` and `validateAbuseRules(` to `ValidateAbuseRules(`. Read the test's imports and fixtures; if it uses radiance-only helpers, inline them.

Add one test for the options form:

```go
func TestValidateAbuseRulesOptionsNeedsRoute(t *testing.T) {
	err := ValidateAbuseRulesOptions(option.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "route")
}
```

- [ ] **Step 2: Run to see it fail**

Run: `go test ./peerrules/`
Expected: FAIL to compile until the source is adapted.

- [ ] **Step 3: Adapt the source**

```go
package peerrules

// (radiance's comments, constants and helpers unchanged)

// ValidateAbuseRules is ValidateAbuseRulesOptions on sing-box options in JSON, such as a peer
// registration's server_config.
func ValidateAbuseRules(optionsJSON string) error {
	ctx := box.BaseContext()
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(optionsJSON))
	if err != nil {
		return fmt.Errorf("parse launch_cfg JSON: %w", err)
	}
	return ValidateAbuseRulesOptions(options)
}

// ValidateAbuseRulesOptions is a defence-in-depth check on the options a peer registration
// returns. (radiance's doc comment for validateAbuseRules follows here unchanged.)
func ValidateAbuseRulesOptions(options option.Options) error {
	if options.Route == nil {
		return errors.New("launch_cfg is missing route block — peer would have no abuse blocking at all")
	}
	var errs []error
	if err := validateAbuseRuleSetTags(options.Route.RuleSet); err != nil {
		errs = append(errs, err)
	}
	if err := validateAbuseRejectRules(options.Route.Rules); err != nil {
		errs = append(errs, err)
	}
	if err := validateStaticRejectCanaries(options.Route.Rules); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
```

Check that lantern-box's root package `box` doesn't import anything that imports `peerrules` (no cycle); `go build ./...` will say.

- [ ] **Step 4: Run tests**

Run: `go test ./peerrules/`
Expected: PASS, including every radiance case.

- [ ] **Step 5: Commit**

```bash
git add peerrules/
git commit -m "peerrules: the peer abuse-rule check, moved from radiance so both can import it"
```

---

### Task 3: Share the pre-Initial junk datagram

**Files:**
- Create: `protocol/internal/quicjunk/junk.go`, `protocol/internal/quicjunk/junk_test.go`
- Modify: `protocol/hysteria2x/outbound.go` (remove `newJunk`, use `quicjunk.New`), `protocol/hysteria2x/export_test.go`, `protocol/hysteria2x/junk_test.go`

**Interfaces:**
- Produces: `func quicjunk.New() ([]byte, error)`: 8 to 64 random bytes with the long-header bit set (`b[0] |= 0x80`).

- [ ] **Step 1: Move the test**

Move `TestNewJunkShape` from `protocol/hysteria2x/junk_test.go` into `protocol/internal/quicjunk/junk_test.go`, calling `New()`. Keep `TestStockServerNeverAnswersJunk` in hysteria2x, calling `quicjunk.New()`. Delete `export_test.go`'s `NewJunk` if nothing else uses it. The quicjunk package has no build tag; it imports only `crypto/rand`.

- [ ] **Step 2: Run to see it fail**

Run: `go test ./protocol/internal/quicjunk/`
Expected: FAIL, undefined `New`.

- [ ] **Step 3: Move `newJunk`**

Cut `newJunk` and its doc comment from `protocol/hysteria2x/outbound.go:162-175` (read it first) into `junk.go` as `New`. In `junkDialer.DialContext`, call `quicjunk.New()`.

- [ ] **Step 4: Run tests**

Run: `go test ./protocol/internal/quicjunk/ && go test -tags with_quic ./protocol/hysteria2x/ && go test -tags with_quic -run TestHysteria2X ./test/e2e/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add protocol/internal/quicjunk protocol/hysteria2x
git commit -m "quicjunk: share the pre-Initial junk datagram with the direct exit client"
```

---

### Task 4: The exit's certificate and pin

**Files:**
- Create: `protocol/unbounded/exit/cert.go`, `protocol/unbounded/exit/cert_test.go`

**Interfaces:**
- Produces (package `exit`, no build tag):
  - `type certificate struct { certPEM, keyPEM string; pin string }`
  - `func newCertificate() (*certificate, error)`
  - `func Pin(leafDER []byte) (string, error)`: the standard base64 SHA-256 of the leaf's PKIX public key.

- [ ] **Step 1: Write the failing test**

```go
package exit

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCertificatePin(t *testing.T) {
	c, err := newCertificate()
	require.NoError(t, err)
	block, _ := pem.Decode([]byte(c.certPEM))
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, x509.ECDSA, leaf.PublicKeyAlgorithm)
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	assert.Equal(t, base64.StdEncoding.EncodeToString(spki[:]), c.pin)
	pin, err := Pin(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, c.pin, pin)

	other, err := newCertificate()
	require.NoError(t, err)
	assert.NotEqual(t, c.pin, other.pin, "a fresh key every time")
}
```

- [ ] **Step 2: Run to see it fail**

Run: `go test ./protocol/unbounded/exit/`
Expected: FAIL, undefined `newCertificate`.

- [ ] **Step 3: Implement**

```go
package exit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// certificate is a volunteer's exit certificate: self-signed, generated at start and kept
// only in memory. Users' apps check it by its pin, which travels in each ICE answer.
type certificate struct {
	certPEM, keyPEM string
	pin             string
}

func newCertificate() (*certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	pin, err := Pin(der)
	if err != nil {
		return nil, err
	}
	return &certificate{
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		pin:     pin,
	}, nil
}

// Pin is the pin users' apps check an exit's certificate by: the standard base64 SHA-256 of
// its PKIX public key, as sing-box's certificate_public_key_sha256 takes it once decoded.
func Pin(leafDER []byte) (string, error) {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
```

The certificate is valid for 30 days. A process that runs longer needs a new one, which Task 5's `Exit` handles by regenerating on rotation (see its `SetRouter`).

- [ ] **Step 4: Run tests**

Run: `go test ./protocol/unbounded/exit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add protocol/unbounded/exit/cert.go protocol/unbounded/exit/cert_test.go
git commit -m "unbounded/exit: an in-memory exit certificate and its pin"
```

---

### Task 5: The exit server

**Files:**
- Create: `protocol/unbounded/exit/exit.go` (`with_quic`), `protocol/unbounded/exit/handler.go` (`with_quic`), `protocol/unbounded/exit/metrics.go`, `protocol/unbounded/exit/stub.go` (`!with_quic`), `protocol/unbounded/exit/exit_test.go` (`with_quic`)

**Interfaces:**
- Consumes: Task 1's `UBClientcore.ExitOptions`; Task 4's `newCertificate`.
- Produces:
  - `type Exit struct` with:
    - `func New(ctx context.Context, logger logger.ContextLogger) (*Exit, error)`
    - `func (e *Exit) SetEnabled(on bool)`: the `direct_exit` flag. Turning it off also ends running sessions.
    - `func (e *Exit) SetRouter(r adapter.ConnectionRouterEx, validUntil time.Time)`: the current exit box's router; nil clears it and ends running sessions.
    - `func (e *Exit) Options() *UBClientcore.ExitOptions`: hooks for `rtcOpt.Exit`, stable for the process's life.
    - `func (e *Exit) Close()`
  - `const InboundTag = "unbounded-exit"`
  - In `!with_quic` builds, `New` returns `nil, ErrUnavailable` (`var ErrUnavailable = errors.New("unbounded exit: built without with_quic")`).

**Behavior:**
- `Ready()` is `enabled && router != nil && now < validUntil`.
- `Params()` returns `false` unless `Ready()`. Otherwise it returns the pin and a fresh password.
- `Serve(ctx, conn, password)`:
  1. Builds a sing-box TLS server config from the certificate, with ALPN `h3`, and starts it.
  2. Builds a `hysteria2.Service[int]` with BBR forced (Global Constraints), `UDPTimeout: 2 * time.Minute`, the handler below, and no masquerade.
  3. Calls `UpdateUsers([]int{0}, []string{password})`, then `Start(conn)`.
  4. Blocks until ctx ends, the conn's reads fail, or the exit is revoked (disabled or its router cleared), then closes the service and conn.
- The handler routes each stream and flow through the router current at that moment, wrapped by `uotrouter.New`:
  - metadata `Inbound: InboundTag`, `InboundType: "unbounded"`, `Source` the session's placeholder address, `Destination` from the request.
  - With no current router, it closes the conn.
  - On close it counts `unbounded.exit.refused` when `rule.IsRejected(err)`, and logs only `failureClass`-style categories at debug, never the destination.
- `OnSession(directExit)` counts `unbounded.exit.sessions{path=ice|webrtc}`. The `reason` label of the spec needs clientcore to report why it chose webrtc, which it doesn't, so it is left out.
- Bytes up and down: count `unbounded.exit.io{direction}` by wrapping the routed conns, using `tracker/metrics/conn.go`'s pattern if it fits; read it first.

- [ ] **Step 1: Write the failing tests**

`exit_test.go` (`//go:build with_quic`) needs a loopback pair standing in for the ICE path. Use two connected UDP sockets on `127.0.0.1`, wrapped so `ReadFrom` returns a fixed placeholder address as clientcore's `icePacketConn` does:

```go
//go:build with_quic

package exit

// placeholderConn is a connected UDP socket as clientcore's ICE packet conn: one peer, and a
// fixed address in place of the real one.
type placeholderConn struct{ *net.UDPConn }

var placeholderAddr = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}

func (c placeholderConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.UDPConn.Read(p)
	return n, placeholderAddr, err
}

func (c placeholderConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.UDPConn.Write(p) }

// pathPair returns two packet conns joined to each other, as the two ends of an ICE path.
func pathPair(t *testing.T) (volunteer, user net.PacketConn) {
	t.Helper()
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	_ = a.Close()
	_ = b.Close()
	va, err := net.DialUDP("udp4", a.LocalAddr().(*net.UDPAddr), b.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	vb, err := net.DialUDP("udp4", b.LocalAddr().(*net.UDPAddr), a.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = va.Close(); _ = vb.Close() })
	return placeholderConn{va}, placeholderConn{vb}
}
```

The router fake records what it was handed and pipes TCP to a local echo listener. Follow `protocol/samizdatreverse/bridge_test.go:40`'s `dialRouter`; read it first.

The test client is a sing-quic `hysteria2.Client` whose dialer returns the user end, with TLS pinned to the exit's pin. Task 7 builds the production one, so here write a small `testClient(t, conn, pin, password)` helper in the test file (`hysteria2.NewClient` with a one-call `N.Dialer`, `tls.NewClient` with `CertificatePublicKeySHA256`).

Tests:

```go
func TestExitNotReadyUntilEnabledAndRouted(t *testing.T) {
	e := newTestExit(t)
	o := e.Options()
	assert.False(t, o.Ready(), "new: off and no router")
	e.SetEnabled(true)
	assert.False(t, o.Ready(), "no router")
	e.SetRouter(&dialRouter{}, time.Now().Add(time.Hour))
	assert.True(t, o.Ready())
	_, _, ok := o.Params()
	assert.True(t, ok)
	e.SetRouter(&dialRouter{}, time.Now().Add(-time.Second))
	assert.False(t, o.Ready(), "registration expired")
	_, _, ok = o.Params()
	assert.False(t, ok)
	e.SetRouter(&dialRouter{}, time.Now().Add(time.Hour))
	e.SetEnabled(false)
	assert.False(t, o.Ready(), "flag off")
}

func TestExitParamsAreFreshPerSession(t *testing.T) {
	e := readyExit(t, &dialRouter{})
	pin1, pw1, ok := e.Options().Params()
	require.True(t, ok)
	pin2, pw2, _ := e.Options().Params()
	assert.Equal(t, pin1, pin2, "one certificate per process")
	assert.NotEqual(t, pw1, pw2)
	raw, err := base64.RawURLEncoding.DecodeString(pw1)
	require.NoError(t, err)
	assert.Len(t, raw, 32)
}

func TestExitServesTCPThroughTheRouter(t *testing.T) {
	r := newEchoRouter(t) // routes TCP to a loopback echo listener and records metadata
	e := readyExit(t, r)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params()
	go e.Options().Serve(context.Background(), vConn, password)
	c := testClient(t, uConn, pin, password)
	conn, err := c.DialConn(context.Background(), M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
	md := r.lastRouted()
	assert.Equal(t, InboundTag, md.Inbound)
	assert.Equal(t, "example.com:80", md.Destination.String())
}

func TestExitRejectsAWrongPasswordOrPin(t *testing.T) {
	for _, tc := range []struct{ name string; wrongPin, wrongPassword bool }{
		{"password", false, true}, {"pin", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := readyExit(t, newEchoRouter(t))
			vConn, uConn := pathPair(t)
			pin, password, _ := e.Options().Params()
			go e.Options().Serve(context.Background(), vConn, password)
			if tc.wrongPassword {
				_, password, _ = e.Options().Params() // another session's password
			}
			if tc.wrongPin {
				other, err := newCertificate()
				require.NoError(t, err)
				pin = other.pin
			}
			c := testClient(t, uConn, pin, password)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.DialConn(ctx, M.ParseSocksaddr("example.com:80"))
			assert.Error(t, err)
		})
	}
}

func TestExitServeEndsWhenRevoked(t *testing.T) {
	for _, revoke := range []func(*Exit){
		func(e *Exit) { e.SetEnabled(false) },
		func(e *Exit) { e.SetRouter(nil, time.Time{}) },
	} {
		e := readyExit(t, newEchoRouter(t))
		vConn, _ := pathPair(t)
		_, password, _ := e.Options().Params()
		done := make(chan struct{})
		go func() { e.Options().Serve(context.Background(), vConn, password); close(done) }()
		revoke(e)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Serve outlived its exit")
		}
	}
}

func TestExitServeEndsWithItsContext(t *testing.T) { /* as above, cancelling ctx */ }

func TestExitCountsRefusals(t *testing.T) {
	// A router whose RouteConnectionEx calls onClose(&rule.RejectedError{...}) and closes the
	// conn: the dial's stream fails, and the refused counter (read through an
	// sdkmetric.ManualReader on a test MeterProvider) is 1. Assert that no log line
	// contains the destination by capturing the logger (sblog.NewDefaultFactory over a buffer).
}

func TestExitForcesBBR(t *testing.T) {
	// A client with SendBPS > 0 (asks for Brutal) still connects, and the server's
	// ServiceOptions has IgnoreClientBandwidth set with ReceiveBPS 0: build the options through
	// the same unexported serviceOptions(...) helper Serve uses and assert on it.
}
```

Write `newTestExit`, `readyExit` and `newEchoRouter` in the test file.

- [ ] **Step 2: Run to see them fail**

Run: `go test -tags with_quic ./protocol/unbounded/exit/`
Expected: FAIL, undefined `New`.

- [ ] **Step 3: Implement `exit.go`**

```go
//go:build with_quic

package exit

// Exit serves a volunteer's direct exits: Hysteria2 over each ICE path broflake hands it, with
// each stream and flow routed into the current exit box as inbound unbounded-exit.
type Exit struct {
	ctx    context.Context
	logger logger.ContextLogger
	cert   *certificate

	mu         sync.Mutex
	enabled    bool
	router     adapter.ConnectionRouterEx
	validUntil time.Time
	// revoked is closed when the exit stops being servable, ending its sessions. A new
	// channel replaces it once it is servable again.
	revoked chan struct{}

	now     func() time.Time
	metrics *exitMetrics
}

const InboundTag = "unbounded-exit"

func New(ctx context.Context, lg logger.ContextLogger) (*Exit, error) {
	cert, err := newCertificate()
	if err != nil {
		return nil, err
	}
	return &Exit{ctx: ctx, logger: lg, cert: cert, revoked: closedChan(), now: time.Now, metrics: newExitMetrics()}, nil
}

func (e *Exit) SetEnabled(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = on
	e.updateRevokedLocked()
}

func (e *Exit) SetRouter(r adapter.ConnectionRouterEx, validUntil time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r != nil {
		r = uotrouter.New(r, e.logger)
	}
	e.router, e.validUntil = r, validUntil
	e.updateRevokedLocked()
}

// updateRevokedLocked closes revoked when the exit can't serve, and opens a new one when it
// can again. Expiry alone doesn't revoke: running sessions end with their path or when
// the registrar clears the router.
func (e *Exit) updateRevokedLocked() {
	servable := e.enabled && e.router != nil
	select {
	case <-e.revoked:
		if servable {
			e.revoked = make(chan struct{})
		}
	default:
		if !servable {
			close(e.revoked)
		}
	}
}

func (e *Exit) ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled && e.router != nil && e.now().Before(e.validUntil)
}

func (e *Exit) currentRouter() adapter.ConnectionRouterEx {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.router
}

func (e *Exit) Options() *UBClientcore.ExitOptions {
	return &UBClientcore.ExitOptions{
		Ready:     e.ready,
		Params:    e.params,
		Serve:     e.serve,
		OnSession: e.metrics.session,
	}
}

func (e *Exit) params() (pin, password string, ok bool) {
	if !e.ready() {
		return "", "", false
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", false
	}
	return e.cert.pin, base64.RawURLEncoding.EncodeToString(b), true
}
```

`serve`:

```go
func (e *Exit) serve(ctx context.Context, conn net.PacketConn, password string) {
	e.mu.Lock()
	revoked := e.revoked
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// pion's conn reads fail once the session ends; watching them ends Serve with it.
	conn = &watchedConn{PacketConn: conn, dead: cancel}

	tlsConfig, err := tls.NewServer(ctx, e.logger, option.InboundTLSOptions{
		Enabled:     true,
		ALPN:        badoption.Listable[string]{"h3"},
		Certificate: badoption.Listable[string]{e.cert.certPEM},
		Key:         badoption.Listable[string]{e.cert.keyPEM},
	})
	if err != nil {
		e.logger.Warn("unbounded exit: TLS config: ", err)
		_ = conn.Close()
		return
	}
	if err := tlsConfig.Start(); err != nil {
		e.logger.Warn("unbounded exit: TLS start: ", err)
		_ = conn.Close()
		return
	}
	defer func() { _ = tlsConfig.Close() }()
	svc, err := hysteria2.NewService[int](serviceOptions(ctx, e.logger, tlsConfig, &handler{exit: e}))
	if err != nil {
		e.logger.Warn("unbounded exit: service: ", err)
		_ = conn.Close()
		return
	}
	svc.UpdateUsers([]int{0}, []string{password})
	if err := svc.Start(conn); err != nil {
		e.logger.Warn("unbounded exit: start: ", err)
		_ = conn.Close()
		return
	}
	select {
	case <-ctx.Done():
	case <-revoked:
	}
	_ = svc.Close()
	_ = conn.Close()
}

// serviceOptions forces BBR: with IgnoreClientBandwidth and no receive rate the service
// answers every client RxAuto, so Brutal is never negotiated.
func serviceOptions(ctx context.Context, lg logger.ContextLogger, tlsConfig tls.ServerConfig, h hysteria2.ServerHandler) hysteria2.ServiceOptions {
	return hysteria2.ServiceOptions{
		Context:               ctx,
		Logger:                lg,
		TLSConfig:             tlsConfig,
		IgnoreClientBandwidth: true,
		UDPTimeout:            2 * time.Minute,
		Handler:               h,
	}
}
```

`watchedConn.ReadFrom` calls `dead()` on any error before returning it. The exact import paths: `tls` is `github.com/sagernet/sing-box/common/tls`, `hysteria2` is `github.com/sagernet/sing-quic/hysteria2`, `badoption` is `github.com/sagernet/sing/common/json/badoption`, `uotrouter` is `github.com/getlantern/lantern-box/protocol/internal/uotrouter`. Check each signature against the module cache before relying on it: `tls.NewServer` (sing-box-minimal `common/tls/server.go:22`), `hysteria2.NewService` (sing-quic `hysteria2/service.go:81`), and whether `tls.ServerConfig` satisfies `ServiceOptions.TLSConfig`'s type.

- [ ] **Step 4: Implement `handler.go`**

```go
//go:build with_quic

package exit

type handler struct{ exit *Exit }

func (h *handler) NewConnectionEx(ctx context.Context, conn net.Conn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	r := h.exit.currentRouter()
	if r == nil {
		_ = conn.Close()
		N.CloseOnHandshakeFailure(conn, onClose, os.ErrClosed)
		return
	}
	var md adapter.InboundContext
	md.Inbound = InboundTag
	md.InboundType = "unbounded"
	md.Source = source
	md.Destination = destination
	r.RouteConnectionEx(log.ContextWithNewID(ctx), h.exit.metrics.countConn(conn), md, h.exit.closed(onClose))
}

func (h *handler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	// the same, through RoutePacketConnectionEx and countPacketConn
}

// closed counts a refusal and logs the failure's class, never its destination.
func (e *Exit) closed(onClose N.CloseHandlerFunc) N.CloseHandlerFunc {
	return func(err error) {
		if err != nil {
			if rule.IsRejected(err) {
				e.metrics.refused()
			}
			e.logger.Debug("unbounded exit: a routed connection failed: ", failureClass(err))
		}
		if onClose != nil {
			onClose(err)
		}
	}
}
```

`failureClass` lives in `protocol/samizdatreverse/failureclass.go:17`, which another package can't call. Move it to `protocol/internal/failureclass` (exported `Of(err error) string`), keep samizdatreverse calling it, and use it here. The move is in this task's commit. Check `N.CloseOnHandshakeFailure` exists in this sing version (`grep -rn "func CloseOnHandshakeFailure" $(go env GOMODCACHE)/github.com/sagernet/sing@*/common/network/`). If it doesn't, call `onClose(os.ErrClosed)` when it isn't nil.

- [ ] **Step 5: Implement `metrics.go` and `stub.go`**

`metrics.go` (no build tag) uses `otel.GetMeterProvider().Meter("lantern-box/unbounded-exit")` once, lazily, so it follows whatever provider the embedder installed:
- `unbounded.exit.sessions{path}` (Int64Counter);
- `unbounded.exit.refused` (Int64Counter);
- `unbounded.exit.io{direction=up|down}` (Int64Counter, unit bytes).

It never takes an address or host as an attribute. `countConn` and `countPacketConn` wrap the routed conns to add bytes.

`stub.go`:

```go
//go:build !with_quic

package exit

var ErrUnavailable = errors.New("unbounded exit: built without with_quic")

type Exit struct{}

func New(context.Context, logger.ContextLogger) (*Exit, error) { return nil, ErrUnavailable }
func (*Exit) SetEnabled(bool)                                    {}
func (*Exit) SetRouter(adapter.ConnectionRouterEx, time.Time)    {}
func (*Exit) Options() *UBClientcore.ExitOptions                 { return nil }
func (*Exit) Close()                                             {}

const InboundTag = "unbounded-exit"
```

`ErrUnavailable` is declared in the `with_quic` build too (in `exit.go`), so callers can compare against it in either build.

- [ ] **Step 6: Run tests**

Run: `go test -tags with_quic -race ./protocol/unbounded/exit/ ./protocol/samizdatreverse/ && go test ./protocol/unbounded/exit/ && go vet -tags with_quic ./protocol/unbounded/exit/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add protocol/unbounded/exit protocol/internal/failureclass protocol/samizdatreverse
git commit -m "unbounded/exit: serve Hysteria2 over ICE paths into the exit box's router"
```

---

### Task 6: The WebRTC-only registrar

**Files:**
- Modify: `node/api.go`
- Create: `node/exit.go`, `node/exit_test.go`

**Interfaces:**
- Consumes: Task 2's `peerrules.ValidateAbuseRules`; Task 5's `exit.Exit` (`SetRouter`).
- Produces:

```go
// ExitConfig configures RunExit.
type ExitConfig struct {
	APIURL     string       // lantern-cloud API base URL, with its version segment
	DeviceID   string
	HTTPClient *http.Client // nil: http.DefaultClient
	// Header, when set, is applied to every request after the defaults, so an app can send
	// its own admission headers (X-Lantern-App and the rest) in place of lantern-box's.
	Header  func(http.Header)
	Version string
	Exit    *exit.Exit
	// NewBox builds and starts the exit box from options JSON. nil: sing-box in ctx.
	NewBox func(ctx context.Context, options string) (ExitBox, error)
	// RotationInterval overrides the interval lantern-cloud gives. 0: lantern-cloud's, or an hour.
	RotationInterval time.Duration
}

type ExitBox interface {
	Router() adapter.ConnectionRouterEx
	Close() error
}

// RunExit keeps a WebRTC-only registration and its exit box current until ctx ends.
func RunExit(ctx context.Context, cfg ExitConfig) error
```

**Behavior:**
- `RunExit` loops: register, then `launchExit`, then wait. The wait ends at the rotation interval or when ctx ends.
- **Register:** `api.registerWebRTC` POSTs `{"mode":"webrtc"}` to `/peer/register` with a 30s timeout, and decodes `{server_config, rotation_interval_seconds}`. An empty `server_config` is an error.
- **`launchExit`:**
  1. Runs `peerrules.ValidateAbuseRules(server_config)`.
  2. Sets `route.auto_detect_interface = true`, so exit traffic leaves on the physical interface and never through the volunteer's own tunnel, as radiance's `ensurePeerOutboundsBypassVPN` does.
  3. Builds the box with `NewBox`.
  4. Calls `cfg.Exit.SetRouter(box.Router(), now + 2*rotation)`.
  5. Closes the previous box after the swap, so running sessions move to the new router for their next stream.
- **Failure** of any step backs off from 15s, doubling to 5m. The current box is kept until its `validUntil`, then closed with `SetRouter(nil, time.Time{})`. A 403 is logged as "lantern-cloud refused this device as a direct exit" with no other detail.
- **ctx done:** `SetRouter(nil, time.Time{})`, then close the box, then return nil.
- **Logs:** never the device ID, the config or response bodies. `apiError`'s body can hold server text, so the registrar logs only the status for it.

`api` changes: add `header func(http.Header)` to `api`, applied at the end of `do`'s header block, and:

```go
type webrtcRegisterRequest struct {
	Mode string `json:"mode"`
}

type webrtcRegisterResponse struct {
	ServerConfig            string `json:"server_config"`
	RotationIntervalSeconds int64  `json:"rotation_interval_seconds"`
}

func (a *api) registerWebRTC(ctx context.Context) (*webrtcRegisterResponse, error) {
	var resp webrtcRegisterResponse
	if err := a.do(ctx, "/peer/register", webrtcRegisterRequest{Mode: "webrtc"}, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	if resp.ServerConfig == "" {
		return nil, errors.New("register: response has no config")
	}
	return &resp, nil
}
```

The default `NewBox` is `sbox.New` then `Start` on the parsed options, as `newSingBox` does, and wraps the box: `Router()` returns `b.Router()`. Check that sing-box's `*box.Box` has `Router() adapter.Router` and that `adapter.Router` includes `adapter.ConnectionRouterEx`; if not, take the router from the box's context with `service.FromContext[adapter.Router]`.

- [ ] **Step 1: Write the failing tests**

`node/exit_test.go` drives `RunExit` against an `httptest.Server` playing lantern-cloud and a fake `NewBox` that records options and returns a box with a recording router. Fixtures: a valid `server_config` (copy the rule shape from `peerrules`' test fixtures) and one missing the abuse rules.

```go
func TestRunExitRegistersWebRTCOnly(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := fakeCloud(t, func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, map[string]any{"server_config": validExitConfig, "rotation_interval_seconds": 3600})
	})
	e := enabledExit(t)
	boxes := &fakeBoxes{}
	ctx, cancel := context.WithCancel(box.BaseContext())
	done := runExitAsync(ctx, ExitConfig{
		APIURL: srv.URL, DeviceID: "dev", Exit: e, NewBox: boxes.new,
		Header: func(h http.Header) { h.Set("X-Lantern-App", "lantern") },
	})
	require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, map[string]any{"mode": "webrtc"}, got)
	assert.Equal(t, "lantern", hdr.Get("X-Lantern-App"), "the caller's headers win")
	assert.Equal(t, "dev", hdr.Get("X-Lantern-Device-Id"))
	assert.True(t, boxes.last().autoDetectInterface, "exit traffic leaves on the physical interface")
	cancel()
	require.NoError(t, <-done)
	assert.False(t, e.Options().Ready())
	assert.True(t, boxes.last().closed)
}

func TestRunExitRefusesConfigWithoutAbuseRules(t *testing.T) {
	// server_config without the rules: no box is built and the exit never becomes ready.
}

func TestRunExitRotatesWithoutAGap(t *testing.T) {
	// RotationInterval 50ms: a second box is built, the router swaps to it, then the first
	// closes, and Ready stays true across the swap.
}

func TestRunExitKeepsServingUntilExpiryWhenRefused(t *testing.T) {
	// First register 200, then 403s: Ready stays true until validUntil (2 × rotation), then
	// goes false and the box closes. Use a short RotationInterval and the exit's clock.
}

func TestRunExitLogsNoSecrets(t *testing.T) {
	// Capture sing-box's log (log.NewFactory over a buffer, as other node tests do; read
	// node_test.go for the pattern) through a 403 with a body naming the device: the output
	// contains neither the device ID nor the body.
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test -tags with_quic ./node/`
Expected: FAIL, undefined `RunExit`.

- [ ] **Step 3: Implement**

Write `node/exit.go` from the behavior above. Keep the backoff constants beside `node.go`'s and reuse them if they're the same values (read `node.go` for `backoff`). The functions are small:
- `RunExit`, which validates cfg and builds `api`;
- `(*exitRunner).loop`;
- `register`;
- `launch`, which parses and validates, sets auto-detect, builds the box and swaps;
- `expire`;
- `close`.

- [ ] **Step 4: Run tests**

Run: `go test -tags with_quic -race ./node/ && go test ./node/`
Expected: PASS. In the `!with_quic` build, `RunExit` still compiles; `exit.Exit` is the stub.

- [ ] **Step 5: Commit**

```bash
git add node/
git commit -m "node: WebRTC-only registration for app volunteers' direct exits"
```

---

### Task 7: The user's side: taking direct exits

**Files:**
- Modify: `option/unbounded.go` (`UnboundedOutboundOptions.DirectExit`), `protocol/unbounded/outbound.go`
- Create: `protocol/unbounded/exitpaths.go` (`with_quic`), `protocol/unbounded/exitpaths_stub.go` (`!with_quic`), `protocol/unbounded/exitpaths_test.go` (`with_quic`)

**Interfaces:**
- Consumes: Task 1's `ExitPath`, `OnExitPath`, `TakeExit`; Task 3's `quicjunk.New`; Task 5's exit server (in tests).
- Produces:

```go
// option/unbounded.go
type UnboundedDirectExitOptions struct {
	// ServerName is the SNI the Hysteria2 handshake sends. Required.
	ServerName string `json:"server_name"`
}
// In UnboundedOutboundOptions:
	// DirectExit, when set, lets this outbound take direct exits from volunteers that offer
	// them. lantern-cloud sets it.
	DirectExit *UnboundedDirectExitOptions `json:"direct_exit,omitempty"`
```

```go
// protocol/unbounded/exitpaths.go
type exitPaths struct { ... }
func newExitPaths(ctx context.Context, lg logger.ContextLogger, serverName string) *exitPaths
func (p *exitPaths) onExitPath(ep UBClientcore.ExitPath)
func (p *exitPaths) takeExit() bool
func (p *exitPaths) dial(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) // errNoExitPath when none is live
func (p *exitPaths) listenPacket(ctx context.Context) (net.PacketConn, error)                          // errNoExitPath likewise
func (p *exitPaths) close()
var errNoExitPath = errors.New("unbounded: no direct exit")
```

**Behavior:**
- **Taking paths.** When `opts.DirectExit != nil` and `ServerName != ""`, `NewOutbound` builds `exitPaths` and sets `rtcOpt.OnExitPath = paths.onExitPath` and `rtcOpt.TakeExit = paths.takeExit`. Without it, neither is set, so the outbound behaves as today. In the `!with_quic` build `newExitPaths` returns nil and the outbound never sets the hooks.
- **`onExitPath`.** It must not block: broflake's consumer worker calls it. It builds a sing-quic `hysteria2.Client` for the path:
  - `Dialer` is a `pathDialer` whose first `DialContext("udp")` returns the path's conn as a `net.Conn`, after writing one `quicjunk.New()` datagram. A later call means the QUIC connection died and sing-quic is redialing, so it marks the path dead, closes the conn and returns an error.
  - `ServerAddress` is `M.ParseSocksaddrHostPort(serverName, 443)`. The dialer ignores it, but sing-quic needs one.
  - `TLSConfig` is `tls.NewClient(ctx, lg, serverName, option.OutboundTLSOptions{Enabled: true, ServerName: serverName, ALPN: ["h3"], CertificatePublicKeySHA256: [pin bytes]})`. The pin bytes are `base64.StdEncoding.DecodeString(ep.CertPin)`. A pin that doesn't decode to 32 bytes drops the path and closes its conn.
  - `Password` is `ep.Password`; `SendBPS` and `ReceiveBPS` are 0 (BBR).
  - The new path is appended as the newest. `unbounded.path{kind=ice, relayed}` is counted.
- **`dial` and `listenPacket`** use the newest live path:
  - TCP: `client.DialConn`. UDP: `bufio.NewBindPacketConn(client.ListenPacket(ctx), destination)`, as hysteria2x does.
  - On error, if the path has never succeeded, it counts a path failure, closes the path and tries the next live one. Once a path has succeeded, an error is the destination's, not the path's: the error is returned, and the path stays.
  - A dead path (its dialer was called again) is removed.
  - With no live path the error is `errNoExitPath`.
- **Back-off.** `takeExit` is false for 10 minutes after 3 path failures in a row; one success resets the count.
- **`Outbound.DialContext` and `ListenPacket`** call `exitPaths` first when it isn't nil. On `errNoExitPath` they fall through to today's path, unchanged. Logging keeps today's shape, plus `"path", "direct_exit"` on direct-exit dials. The user's own app already logs its destinations at debug, so this adds nothing new, but it must log no pin, password or path address.
- **`Close`** calls `paths.close()` after `ui.Stop()`, closing every path's client and conn.

- [ ] **Step 1: Write the failing tests**

`exitpaths_test.go` (`with_quic`) uses Task 5's real `exit.Exit` as the volunteer, over the loopback `pathPair` (copy the helper into this package's test file), with an echo router. Hand the user end to `onExitPath` as an `ExitPath`.

```go
func TestExitPathsDialThroughTheNewestPath(t *testing.T) {
	v := newVolunteerExit(t) // exit.Exit, enabled, with an echo router
	p := newExitPaths(context.Background(), testLogger(), "www.example.com")
	t.Cleanup(p.close)
	p.onExitPath(v.path(t)) // serves one session, returns the user's ExitPath
	conn, err := p.dial(context.Background(), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
}

func TestExitPathsSendJunkFirst(t *testing.T) {
	// Wrap the volunteer end to record datagrams: the first one has the long-header bit and
	// 8–64 bytes and is not a QUIC Initial (reuse isQUICv1Initial from test/e2e by copying it),
	// and the session still connects.
}

func TestExitPathsDropAPathWithAWrongPin(t *testing.T) {
	// ExitPath with another certificate's pin: dial fails with errNoExitPath, the path's conn is
	// closed, and one failure is counted.
}

func TestExitPathsBackOffAfterRepeatedFailures(t *testing.T) {
	// Three bad-pin paths: takeExit is false; with the clock moved 10 minutes on it is true;
	// a good path's success resets the count.
}

func TestExitPathsKeepAPathAfterADestinationError(t *testing.T) {
	// Router refuses one destination (RejectedError): after one good dial, a refused dial
	// returns an error and the path stays live for the next dial.
}

func TestExitPathsRemoveADeadPath(t *testing.T) {
	// Close the volunteer's conn: within the QUIC idle timeout (shorten it if sing-quic exposes
	// it, otherwise use a 40s test timeout and t.Skip under -short), the next dial returns
	// errNoExitPath and the path's conn is closed.
}

func TestOutboundTakesExitsOnlyWhenConfigured(t *testing.T) {
	// NewOutbound without DirectExit leaves rtcOpt.OnExitPath and TakeExit nil; with
	// DirectExit{ServerName: "x"} both are set.
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test -tags with_quic ./protocol/unbounded/ -run 'ExitPaths|TakesExits'`
Expected: FAIL, undefined `newExitPaths`.

- [ ] **Step 3: Implement**

`exitpaths.go` holds `exitPaths`, `exitPath{client *hysteria2.Client; conn net.PacketConn; succeeded, dead atomic.Bool}`, `pathDialer`, and `packetConnAsConn`. That last is a `net.Conn` over a one-peer packet conn: `Read` is `ReadFrom`, `Write` is `WriteTo` with the placeholder `RemoteAddr`, and `RemoteAddr` returns `&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}`. Use the same mutex and clock (`now func() time.Time`) pattern as `watchdog.go`. Then wire `outbound.go` as described.

- [ ] **Step 4: Run tests**

Run: `go test -tags with_quic -race ./protocol/unbounded/... && go test ./protocol/unbounded/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add option/unbounded.go protocol/unbounded/
git commit -m "unbounded: take volunteers' direct exits over Hysteria2, falling back to today's path"
```

---

### Task 8: Lamplighter nodes as direct exits

**Files:**
- Modify: `option/unbounded.go` (`UnboundedVolunteerOptions.DirectExit bool`), `protocol/unbounded/volunteer.go`, `node/node.go` (`applyOperatorLimits`)
- Test: `protocol/unbounded/volunteer_test.go`, `node/node_test.go`

**Interfaces:**
- Consumes: Task 2's `peerrules.ValidateAbuseRulesOptions`; Task 5's `exit.New`, `SetEnabled`, `SetRouter`, `Options`.
- Produces: `UnboundedVolunteerOptions.DirectExit bool \`json:"direct_exit,omitempty"\``.

**Behavior:**
- **The volunteer service.** With `DirectExit`, `Start` (at `StartStateStart`) takes the box's router with `service.FromContext[adapter.Router](v.ctx)`, then:
  - builds `exit.New(v.ctx, v.logger)`;
  - calls `SetEnabled(true)` and `SetRouter(router, time.Now().Add(100*365*24*time.Hour))`. The node's rotation rebuilds the box, so this exit lives as long as its box.
  - sets `v.rtcOpt.Exit = e.Options()` before `NewBroflake`.
  
  If `exit.New` fails (for example `ErrUnavailable` without `with_quic`), it warns once and serves today's path. `Close` calls `e.Close()` after `ui.Stop()`.
- **`node.launch`, through `applyOperatorLimits`.** When the volunteer has `DirectExit` and `peerrules.ValidateAbuseRulesOptions(opts)` fails, it clears `DirectExit` and warns "lantern-cloud's rules failed the abuse check, so this node won't exit Unbounded traffic directly". The rules are not logged. The node's allowlist and abuse rules are global in its box, so the volunteer's exit streams meet the same rules as its samizdat inbound.
- **The tracker catch.** `cmd/cmd_run.go:101-107` installs a `clientcontext` manager matching every inbound, which reads a client-info frame at the start of each routed TCP conn for up to 5s. `lantern-box run` with a direct-exit volunteer would stall exit streams. Exclude `exit.InboundTag` from its bounds: read `tracker/clientcontext` for how `MatchBounds` excludes.

- [ ] **Step 1: Write the failing tests**

```go
// volunteer_test.go
func TestVolunteerDirectExitHooksIntoBroflake(t *testing.T) {
	// Build a box (newVolunteerBoxContext) with the volunteer service, direct_exit true, and a
	// route with the abuse fixture. After Start, v.rtcOpt.Exit is non-nil and Ready() is true.
	// With direct_exit false, v.rtcOpt.Exit is nil.
}

// node_test.go
func TestApplyOperatorLimitsDropsDirectExitWithoutRules(t *testing.T) {
	// A server_config whose volunteer has direct_exit but whose route lacks the abuse rules:
	// the returned options' volunteer has DirectExit false. With the rules it stays true.
}
```

For `cmd/cmd_run.go`, add a test beside the existing clientcontext tests that a conn routed with `Inbound: "unbounded-exit"` isn't read for client info. If the bounds are built inline in `cmd`, extract a tiny function to test.

- [ ] **Step 2: Run to see them fail**

Run: `go test -tags with_quic ./protocol/unbounded/ ./node/ ./cmd/ ./tracker/clientcontext/`
Expected: FAIL.

- [ ] **Step 3: Implement**

Follow the behavior above. Keep `NewVolunteer`'s validation order. `DirectExit` doesn't change which options are required, because a node's volunteer still relays webrtc sessions to the egress.

- [ ] **Step 4: Run tests**

Run: `go test -tags with_quic -race ./protocol/unbounded/ ./node/ ./cmd/ ./tracker/clientcontext/ && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add option/unbounded.go protocol/unbounded/volunteer.go protocol/unbounded/volunteer_test.go node/ cmd/ tracker/
git commit -m "unbounded: Lamplighter nodes exit Unbounded traffic directly under their own rules"
```

---

### Task 9: End-to-end test with the real freddie

**Files:**
- Create: `test/e2e/direct_exit_test.go` (`//go:build with_quic`), `test/e2e/testdata/direct_exit_server.json`

**Interfaces:**
- Consumes: everything above; the e2e helpers in `test/e2e/unbounded_test.go` (`freddie.New`, `startEgress`, `startWidget`, `startConsumers`, `proxiedGetOK`, `freePort`). Read that file first.

The fixture is an exit box's options:
- outbound `direct`;
- `route.rule_set` with the four abuse tags as `inline` rule sets (no downloads), each matching a domain only the test uses, such as `malware.test`;
- unconditional reject rules for those sets, for `ip_cidr` `10.0.0.0/8` and for `port` `25`;
- `final: "direct"`.

It must pass `peerrules.ValidateAbuseRules`, which the test asserts first. It must not reject loopback, so the test's origin on `127.0.0.1` is reachable.

The volunteer is a raw broflake widget, as `startWidget` builds it, with `rtcOpt.Exit` from an `exit.Exit` whose router is a box built from the fixture. The user is a consumer box whose unbounded outbound has `direct_exit: {"server_name": "www.example.com"}`.

- [ ] **Step 1: Write the tests**

```go
func TestDirectExitE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	// freddie, egress (it must see no traffic for direct exits: count its SOCKS dials),
	// an HTTP origin and a UDP echo on 127.0.0.1, the exit box from the fixture, the widget
	// with Exit, and one consumer with direct_exit.
	t.Run("http via the volunteer, not the egress", func(t *testing.T) { /* proxiedGetOK; egress dial count 0 */ })
	t.Run("udp echo", func(t *testing.T) { /* SOCKS5 UDP associate through the consumer's mixed inbound */ })
	t.Run("10.0.0.0/8 refused", func(t *testing.T) { /* a request to 10.255.255.1:80 fails fast */ })
	t.Run("port 25 refused", func(t *testing.T) { /* 127.0.0.1:25 fails */ })
	t.Run("abuse rule set refused", func(t *testing.T) { /* malware.test fails */ })
}

func TestDirectExitFallsBackToEgress(t *testing.T) {
	for _, tc := range []struct{ name string; enabled bool; router bool; consumerDirectExit bool }{
		{"flag off", false, true, true},
		{"no registration", true, false, true},
		{"old consumer", true, true, false},
	} {
		// each: proxiedGetOK succeeds and the egress saw the dial
	}
}
```

The wrong pin and password, BBR and junk-first are covered in Tasks 5 and 7. A test with freddie adds nothing to them.

- [ ] **Step 2: Run**

Run: `go test -tags with_quic -run 'DirectExit' -race ./test/e2e/ -timeout 10m`
Expected: PASS. If a subtest flakes on timing, fix the wait (bounded polling), not the assertion.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/direct_exit_test.go test/e2e/testdata/direct_exit_server.json
git commit -m "e2e: direct exits through the real freddie, refusals and fallbacks"
```

---

### Task 10: The benchmark

**Files:**
- Create: `test/e2e/direct_exit_bench_test.go` (`//go:build with_quic && bench`)

The spec asks for a benchmark that compares the ICE plus Hysteria2 path with today's relayed path. It measures:
- bulk throughput;
- stream-open latency;
- throughput while another stream loses packets;

on lossy links (0, 2 and 5% loss, 50–200 ms RTT). The e2e harness runs on loopback, so apply loss and delay with a packet-conn shim. Wrap both paths' packet conns:
- the volunteer side of the ICE path: `Exit.Serve`'s conn;
- the widget's and consumer's WebRTC nets: `rtcOpt.Net`, through a pion `transport.Net` wrapper.

The shim drops a set fraction of datagrams and delays the rest. A real TURN relay is measured by hand at rollout (spec Rollout step 3), not here.

- [ ] **Step 1: Write the shim and its test**

```go
// lossyConn drops each datagram with probability loss and delays the rest by delay.
type lossyConn struct {
	net.PacketConn
	loss  float64
	delay time.Duration
	rand  *rand.Rand
}

func TestLossyConnDropsAboutItsShare(t *testing.T) { /* 10k writes at 5%: 400–600 dropped */ }
```

- [ ] **Step 2: Write the benchmarks**

`BenchmarkPath/{ice,webrtc}/loss={0,2,5}/rtt={50,200}/{bulk,open,hol}` report MB/s and ms with `b.ReportMetric`. `bulk` is 32 MB through one stream. `open` is the time to the first byte of a new stream. `hol` runs two streams, with a 5% loss burst applied to one stream's direction only.

- [ ] **Step 3: Run once**

Run: `go test -tags 'with_quic bench' -run '^$' -bench BenchmarkPath -benchtime 1x ./test/e2e/ -timeout 30m`
Expected: completes. Paste the table into the PR description.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/direct_exit_bench_test.go
git commit -m "e2e: benchmark direct exits against today's relayed path"
```

---

## After this plan

- **lantern-cloud** implements the contract above: `mode: "webrtc"`, refusals, the `direct_exit` flag, the outbound's `direct_exit.server_name`, and the node volunteer's `direct_exit`.
- **radiance**, once a lantern-box release has this:
  - runs `node.RunExit` while the widget runs, with its HTTP client, device ID and app headers;
  - sets `rtcOpt.Exit` from the `exit.Exit`, and calls `SetEnabled` from config's `direct_exit` feature;
  - switches `peer/validate.go` to `peerrules`.
- **lantern** then bumps radiance.
