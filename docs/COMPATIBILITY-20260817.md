# Upgrade and wire-compatibility evidence — 2026-08-17

## Scope

This is the local isolated compatibility gate for the public-readiness tree.
The release-candidate workflow and review pull request must record the final
full commit SHA; do not infer an approved commit from this dated report alone.

The previous deployed binary was `roadmap-cea99ea` at commit `cea99ea`, built
with Go 1.25.13 and speaking wire protocol 3. The candidate tree was built with
the same Go version. Tests used fresh private test credentials, loopback-only
TCP/UDP listeners, one numeric public HTTP destination, and persistent DNS
through one SOCKS5 UDP association. No live service or public listener was
modified.

## Patch-upgrade matrix

| Server | Client | HTTP | Persistent UDP | Result |
| --- | --- | --- | --- | --- |
| Previous wire 3 | Candidate wire 3 | expected 301 received | 3/3 | Pass |
| Candidate wire 3 | Previous wire 3 | expected 301 received | 3/3 | Pass |
| Candidate wire 3 | Candidate wire 3 | expected 301 received | 3/3 | Pass |

The two mixed rows exercise each possible first endpoint in a one-at-a-time
patch upgrade. Returning to the previous/previous deployed pair is independent
of credentials and uses the documented atomic binary rollback. The final
candidate must rerun this matrix or demonstrate equivalent native/integration
evidence if any wire, handshake, frame, SOCKS, or UDP code changes afterward.

## Incompatible-version refusal

A synthetic peer was built from the candidate tree with only
`internal/protocol.Version` changed from 3 to 2. This retains the current TLS
identity and ALPN so the connection reaches the frame parser rather than being
rejected earlier for an unrelated historical TLS difference.

The wire-2 client was unable to open a destination through the wire-3 server.
Refusal completed in less than one second, and the server emitted:

```text
unsupported wire version 2 (this build speaks 3)
```

The older historical wire-2 binary additionally fails closed during TLS because
it predates the current ALPN. That is expected but is not used as evidence for
the frame-version diagnostic; the controlled same-ALPN peer above is.

Unit coverage mutates otherwise-valid headers to `Version-1` and `Version+1`,
asserts `UnsupportedVersionError`, and verifies both peer and local numbers.
There is no version downgrade and no version-2 data path in the candidate.
