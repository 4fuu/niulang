# Primary-path high-port validation — 2026-08-17

## Cell identity and limits

| Field | Value |
| --- | --- |
| Access path | Opaque path A; maintainer-controlled physical long-haul uplink |
| Egress | Opaque primary egress A |
| Port class | High port, shared TCP/UDP |
| Client | macOS arm64, Clash/mihomo to loopback SOCKS5 |
| Deployed build | `roadmap-cea99ea`, commit `cea99ea`, Go 1.25.13, wire 3 |
| Fault injection | None |

This is one real-path mechanism cell, not an independent-network diversity
pass and not a 24-hour soak. Provider, subscriber, host, and address identifiers
are deliberately omitted. The complete raw bundles are retained privately by
the maintainer with mode 0700; the public aggregate contains no response body,
destination-observed address, credential, or host identifier.

## Pre-rotation baseline

The first run lasted 600.396 seconds from 08:43:57Z to 08:53:57Z:

- persistent SOCKS5 UDP: 111/111 successful DNS replies, including the final
  five, with no association reconnect or rescue failure;
- independent verified HTTPS: 10/10 successful;
- flows: 11 started, 11 completed, zero failed, zero active at settlement;
- zero fallback, timeout, lane failure/replacement, and replay bytes;
- client descriptors: 16 to 16; RSS: 23,088 to 23,536 KiB.

The SHA-256 of `summary.json` is
`1b0358e10e1d25212dd3db1642cb2e5d1fa78804c5997d01338bef709e77b78b`.
The bundle's `SHA256SUMS` file hashes to
`39561b740f2ab012a2cbe58b9a0c23e6aee38826298e179c9797ff6a941a4686`.

## Credential rotation and post-rotation run

The paired TLS root/leaf/key and 384-bit session secret were replaced in a
coordinated maintenance window. HTTPS, a five-query persistent UDP association,
direct TLS chain/name/fingerprint verification, service state, metrics, and
deployed ownership/modes all passed before the second soak.

The post-rotation run lasted 600.396 seconds from 09:03:14Z to 09:13:14Z:

- persistent SOCKS5 UDP: 114/114 replies, including the final five, with no
  association reconnect or rescue failure;
- independent verified HTTPS: 10/10 successful;
- flows: 11 started, 11 completed, zero failed, zero active at settlement;
- zero fallback, timeout, lane replacement, and replay bytes;
- client descriptors: 16 to 16; RSS: 20,704 to 23,520 KiB.

Four `lane_failures_total` increments coincided with completed independent
HTTPS flows. No request, association, or byte stream failed and no replacement
was opened. Investigation reduced this to a close-classification defect: after
delivering a valid peer CLOSE, the lane reader classified the following stream
EOF as a transport failure before the receive loop recorded normal completion.
The release-readiness tree recognizes EOF after a CLOSE from that same reader
as a normal half-close, preserves the final-ACK/write direction, and contains a
deterministic regression test. An EOF without CLOSE still retires the failed
lane immediately. The deployed `cea99ea` binary in this cell predates that fix,
so the observation remains in the record rather than being relabeled as zero.

The SHA-256 of `summary.json` is
`f9a35d32031d1cd9a71fd8248df946d633d756cd79d83301e60fd5064df83edf`.
The bundle's `SHA256SUMS` file hashes to
`0b8c7daad686f88e8a921137f77d3932332ee05df5b80ca69d2385d701df06b6`.

After hardening the harness to require a matching transaction, standard query
opcode, non-truncated zero-rcode reply, one question, and at least one answer,
a 60.440-second confirmation completed 27/27 UDP and 3/3 HTTPS probes with
zero lane failures and all resources settled. Its `summary.json` SHA-256 is
`c7b88782b5260f03f92ce8c409d10de8ba478fe7d9bfcd343ff0523053cfce0a`;
its `SHA256SUMS` file hashes to
`5a954a32263467ddb880d64855d6a6ebbbc5564c359ee22433bd2c07a79fa090`.

## Applicable acceptance result

There was no crash, panic, data corruption, failed flow, timeout, fallback,
reconnect, rescue failure, descriptor growth, active-flow leak, or replay-byte
leak. HTTPS certificate validation and strict DNS transaction/response
validation were enabled. The cell passes its bounded mechanism criteria subject
to a candidate rerun of the lane-close regression. It does not satisfy any missing residential,
mobile, managed-network, second-egress, port-443, sleep/wake, handoff, PMTU, or
24–72-hour matrix cell.
