# Security and resource review

This review covers the current paired deployment: a local SOCKS5 agent under
the operator's control and one Internet-facing egress agent. It is not a claim
of third-party audit or a production-readiness certificate.

## Trust boundaries

- The egress accepts traffic only after TLS 1.3 and the queqiao HELLO HMAC.
  The certificate authenticates the egress to the client; the pre-shared
  secret authenticates the client to the egress.
- The local SOCKS5 listener has no user authentication. It must bind to
  loopback or another access-controlled interface. Anyone who can reach it can
  use the fixed egress and consume its session budget.
- A holder of the shared secret is trusted to use the proxy, but is not trusted
  to supply well-formed frames or bounded identifiers. Parser, admission, and
  memory limits still apply after authentication.
- Destination hosts and the network path are untrusted. TLS protects the outer
  payload and handshake; endpoints still observe normal connection metadata.

## Controls reviewed

| Area | Current control | Verification |
| --- | --- | --- |
| Transport identity | TLS 1.3, verified certificate name/root, fixed ALPN | configuration and integration tests |
| Client authentication | HMAC-SHA-256 over timestamp, random nonce, session/lane identity, and kind | session unit tests; constant-time MAC comparison |
| Replay | five-minute timestamp window plus a ten-minute bounded nonce cache | replay-guard tests |
| Wire allocation | payload length rejected before allocation; hard 1 MiB protocol ceiling and 256 KiB service default | protocol unit tests and fuzz targets |
| Sessions | local and remote admission semaphores; configured ceiling capped at 65,536 | configuration/admission tests |
| QUIC connections | accepted connections capped at `MaxSessions`, including connections that never authenticate or open a stream | `TestQUICConnectionsHaveAnAdmissionBound` |
| Lanes and replay memory | fixed per-flow lane budget, 16,384 retained chunks, 64 MiB sender ceiling, 128 MiB reassembly ceiling | scheduler/reassembly tests |
| UDP rescue | cryptographic 128-bit, single-use token; 30-second expiry; at most 256 retained relay sockets | UDP relay and repeated rescue tests |
| Lifetime | bounded handshake, path idle, application idle, maximum flow lifetime, lane recovery attempts, and completion tombstone | lifecycle and failure-injection tests |
| Egress SSRF | DNS resolved at the egress to a concrete address; private, loopback, link-local, multicast, documentation, benchmark, and shared ranges rejected by default | destination policy tests |
| Service account | packaged systemd unit uses a dedicated user, no new privileges, strict filesystem protection, syscall architecture restriction, and explicit writable paths | deployment template review |
| Parser robustness | all discovered fuzz targets run weekly; malformed frames, ACK ranges, and coded symbols have dedicated fuzzers | `.github/workflows/deep.yml` |
| Concurrency | complete race suite runs weekly | `.github/workflows/deep.yml` |
| Known vulnerabilities | pinned `govulncheck` performs reachability-aware scans against Go's official vulnerability database weekly and before publishing | deep and release workflows |
| Fallback soak | repeated UDP removal, TCP rescue, relay-source preservation, UDP restoration, and race-detector runs | `scripts/fallback_soak.sh` and the deep workflow |

The review found one remotely reachable resource gap: QUIC connection objects
were admitted before the per-stream session semaphore, so a peer that never
opened a stream could create an unbounded number of server goroutines and live
connections. Accepted QUIC connections now have their own `MaxSessions`-sized
admission semaphore and excess connections are closed immediately.

The first pinned vulnerability scan also found 11 reachable standard-library
advisories in the workstation's Go 1.25.7 toolchain. All are fixed in Go
1.25.13, so `go.mod` now requires that patch release or newer. This is a build
input, not only a CI preference: Go's toolchain selection upgrades an older
local command before compiling release binaries.

## Residual risks and operating requirements

- One pre-shared secret represents the whole client side. There are no
  per-user identities, quotas, revocation list, or online secret rotation.
- Per-flow buffering is bounded, but the aggregate ceiling is the product of
  `MaxSessions` and the per-flow limits. Operators must set `MaxSessions` to a
  value the host can fund; the command default is 1,024, not a sizing promise.
- The metrics endpoint has no authentication. Bind it to loopback or leave it
  disabled. Logs and metrics omit application plaintext and the shared secret,
  but destinations remain observable to the two endpoints and their kernels.
- Secret bytes are not locked in memory. Configuration files must be readable
  only by the service account, and core dumps should remain disabled by the
  service manager.
- The egress policy prevents connections to non-public resolved addresses by
  default, but enabling `--allow-private-destinations` intentionally removes
  that boundary and must be confined to a benchmark environment.
- This is a manual code/configuration review, not an independent cryptographic
  assessment. The real intermittent-block and bounded mixed soak are recorded
  in `RELEASE-HARDENING-20260817.md`; broader path/middlebox diversity and
  third-party review remain external qualifications.

## Release verification

Before a tagged release, run the normal and deep workflows, verify
`SHA256SUMS`, and retain the previous binary as described in `RELEASING.md`.
A release report should record:

```sh
go test -timeout 50m ./...
go test -race -timeout 110m ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
```

The scheduled deep workflow additionally discovers and smokes every fuzz
target, so adding a fuzzer does not require editing a static target list.
