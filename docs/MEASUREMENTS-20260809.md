# Development measurements — 2026-08-09

These are development results, not a claim of production readiness.

## Controls and route isolation

The real endpoint is `23.135.236.244:12443` with TLS SNI
`icourses-dev.01.me`. `icourses-dev.01.me` itself resolves to a Clash fake IP
on this host and must not be used as the socket endpoint.

Clash Verge TUN had installed a default route through `198.18.0.1`. Numeric
endpoint tests performed before that was discovered were invalid because the
outer PEP connection was captured by the live Clash route. Valid tests bound
the local outer socket to the physical address `192.168.3.66` using
`--local-address`; no Clash profile was edited or switched. The development
service alone listened on TCP and UDP `:12443`.

The HTTP object was the fixed 10 MiB
`https://cachefly.cachefly.net/10mb.test`. Each trial launched concurrent
SOCKS flows, measured wall-clock aggregate goodput, and required the complete
10 MiB response for a successful flow. The corrected harness clears
`NO_PROXY` and records curl's exit status; a 200 response with a timed-out
partial body is not a success.

## Valid TCP matrix

Three randomized repetitions were run for each concurrency level. Values are
the median aggregate Mbps across the three repetitions:

| Independent SOCKS flows | 1 | 2 | 4 | 8 |
|---:|---:|---:|---:|---:|
| TCP/TLS aggregate Mbps | 4.70 | 6.90 | 17.61 | 16.49 |
| Complete flows | 3/3 | 3/3 | 3/3 | 3/3 |

This is an independent-flow control, not a single-flow multipath result. It
shows that the path is materially flow-sensitive, which is a prerequisite
for testing a PEP, but it does not prove that lane striping will reproduce
the gain.

## QUIC prototype observations

The stock quic-go controller was tested with a 1200-byte initial packet size
and path-MTU probing disabled. A one-lane 10 MiB response transferred only
about 1.3–1.5 MiB before the 60-second cap. With two initial lanes and the
PIAS controller, the server observed actual data on three lanes, but the
flow still transferred only about 2.0 MiB in 60 seconds. A forced eight-lane
run was interrupted after approximately 105 seconds with 4.0 MiB delivered;
server/client counters showed roughly 0.46–0.72 MiB on each active lane.

The packet trace confirms that the MTU mitigation kept packets at 1200 bytes.
The remaining limitation is congestion control/loss recovery, not merely
frame reordering. These censored runs must not be summarized as throughput
successes.

## QUIC request latency pilot

Ten fresh one-lane requests per endpoint were attempted through the physical
QUIC lane. Google `generate_204` succeeded 10/10 with median 1.68 s and p95
2.58 s. `api.openai.com/v1/models` (401 is an expected application response)
succeeded 6/10; successful median was 2.01 s and p95 4.46 s. Four requests
timed out at 20 s. The sample is small and highly variable; it is a diagnostic
signal, not a product SLA.

## Interpretation

The prototype currently proves:

- authenticated TLS/TCP and QUIC lanes can reach the fixed US egress;
- destination DNS and dialing happen at the US server;
- a single logical flow can be reordered across independently authenticated
  lanes; and
- lane-byte counters can verify actual striping.

It currently does **not** prove:

- higher single-flow throughput than TUIC/Hysteria 2;
- acceptable interactive tail latency under bulk load;
- seamless mid-session UDP-to-TCP recovery; or
- safety for an unattended production Clash profile.

## TCP fallback smoke test

For a separate smoke test, the isolated listener was temporarily run with
`--transport tcp` (UDP was absent), while the client used `--transport auto`.
A 64 KiB HTTP range request completed in 2.99 s, and the client completion
counter identified the lane as `Kind: tcp`. The normal dual TCP/UDP systemd
service was restored and verified active immediately afterward. This verifies
new-flow fallback selection only; it is not a seamless mid-session recovery
test.

The next transport experiment should compare a maintained BBR/rate-based
QUIC congestion controller against this stock CUBIC baseline before changing
the lane scheduler again.
