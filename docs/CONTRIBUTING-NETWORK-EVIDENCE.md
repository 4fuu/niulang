# Contributing network evidence

Queqiao is intended to evolve from observations across networks the maintainers
cannot access. A report that disproves a design assumption is as valuable as a
performance win.

Start with [Benchmarking](BENCHMARKING.md) for the tools and exact command
forms. Use [Field validation](FIELD-VALIDATION.md) when a result should count
toward release qualification.

## What to report

Describe the path without publishing sensitive infrastructure:

- client and gateway region, access class, and whether the gateway is fixed;
- transport availability (UDP, TCP, ports, PMTU, NAT or middlebox behavior);
- direction, RTT range, offered and delivered rate, loss percentage, burst
  structure, and whether a capacity knee is visible;
- Queqiao commit, wire version, Go version, OS/architecture, and exact command;
- baseline implementation and controller, with both systems measured in the
  same alternating time window; and
- completion counts and distributions, not only the best or median sample.

Do not infer “random loss” from a percentage alone. A 15% or 42% loss rate may
be independent erasure, queue overflow, wireless contention, shaping, a broken
route, or a measurement artifact. Record enough time-series or conditional-loss
data to distinguish them.

## Evaluate one design three ways

Do not configure or describe three Queqiao architectures. Exercise the same
transport and report the workload families that apply:

| Family | Suggested observations |
| --- | --- |
| Short-lived | fresh and warm `curl`/API/page-resource setup, first byte, completion, and failure tail |
| Interactive | SSH request latency and voice/video-style packet latency, jitter, delivery, and tail during contention |
| Bulk | download and upload goodput, completion, physical/logical byte ratio, CPU, memory, descriptors, and recovery |

If a change helps one family and damages another, report both. That tradeoff is
part of the result.

## Make comparisons interpretable

- Route all candidates over the same physical interface and verify that a TUN,
  existing proxy, or fake-IP route did not capture the supposedly direct path.
- Compare complete stacks. BBR is a congestion controller; identify the proxy,
  framing, TLS/TCP or QUIC transport, recovery, and BBR implementation around it.
- Alternate candidates rather than running them in separate path windows.
- Keep workload size, endpoint, direction, controller, trial count, and offered
  load matched unless the experiment explicitly changes one of them.
- Preserve failed and incomplete trials. Do not silently discard stalls.
- State whether loss is injected, endpoint-emulated, or observed on a real
  access network. Only the last can fill a field-validation cell.

## Protect users and infrastructure

Before publishing, remove:

- invitations, profiles, private keys, certificates, tokens, and provider state;
- public subscriber or gateway addresses when they are not essential;
- private hostnames, usernames, local paths, packet payloads, and user traffic;
- raw logs that contain destinations or other users' metadata.

Prefer semantic labels such as `residential-path-a` and report aggregates. If a
maintainer needs sensitive raw evidence, arrange a private transfer; do not put
it in a public issue.

## Suggested report shape

1. Question or suspected counterexample
2. Path and route-isolation controls
3. Exact builds and configuration
4. Short-lived, interactive, and/or bulk procedure
5. Raw summary and distributions
6. Interpretation, with alternative explanations
7. Limits: what the result does not establish

Add a redacted field record only after it meets the provenance and acceptance
rules in [Field validation](FIELD-VALIDATION.md). Otherwise share it as a
development report and label it accordingly.
