# Queqiao documentation

> [!NOTE]
> This index is the public entry point for Queqiao documentation. Files linked
> under **Current documentation** describe protocol version 1 and the current
> source tree. Dated development notebooks live under [`archive/`](archive/)
> and must not be used as current operational guidance.

## Start here

| If you want to… | Read |
| --- | --- |
| Understand why the project exists | [Vision and design principles](VISION.md) |
| Check maturity and supported scope | [Project status](STATUS.md) |
| Install a provider and client | [Deployment guide](DEPLOYING.md) |
| Understand the transport | [Current design](DESIGN.md), then [architecture](ARCHITECTURE.md) |
| Measure a path or compare transports | [Benchmarking](BENCHMARKING.md) |
| Contribute observations from another network | [Contributing network evidence](CONTRIBUTING-NETWORK-EVIDENCE.md) |

## Current documentation

### Concepts and protocol

- [Vision and design principles](VISION.md) — the durable problem statement,
  assumptions, and rules that should survive mechanism changes.
- [Current design](DESIGN.md) — why the present controller, recovery, pacing,
  and isolation mechanisms follow from measured path behavior.
- [Architecture](ARCHITECTURE.md) — components, unified flow path, trust
  boundaries, fallback, and resource bounds.
- [Protocol version 1](PROTOCOL.md) — current wire framing and security
  invariants.
- [Known limitations](KNOWN-LIMITATIONS.md) — product, topology, privacy, and
  operational limits.

### Use and operation

- [Deployment guide](DEPLOYING.md) — provider setup, enrollment, Clash/mihomo,
  service installation, upgrades, and troubleshooting.
- [Runtime logging](LOGGING.md) — log locations, retention, telemetry, and
  operational controls.
- [Mobile clients](MOBILE.md) — Android/iOS build, distribution, and release
  constraints.
- [Android export mode](ANDROID-EXPORT.md) — the released Android app as a
  local SOCKS5 endpoint, the per-app bypass every consumer client must apply,
  and v2rayNG/mihomo/sing-box configuration.
- [Releasing](RELEASING.md) — local packages, candidate workflows, installation,
  and rollback.

### Evaluation and qualification

- [Project status](STATUS.md) — what is implemented, what the evidence supports,
  and what remains open.
- [Benchmarking](BENCHMARKING.md) — reproducible path and workload measurement.
- [Field validation](FIELD-VALIDATION.md) — required real-network diversity
  matrix and acceptance criteria.
- [Field-result index](field-results/README.md) — current protocol-1 field
  evidence; currently awaiting a complete campaign.
- [Path characterization, 2026-08-13](PATH-CHARACTER-20260813.md) — the
  open-loop measurement that exposed the erasure floor and congestion knee on
  the motivating path.

### Project governance

- [Roadmap](ROADMAP.md) — implemented stages and remaining external
  qualification.
- [Public-release checklist](RELEASE-CHECKLIST.md) — authority for preview and
  production-ready claims.
- [Production design criteria](PRODUCTION-DESIGN.md) — security, operational,
  and transport gates.
- [Contributing guide](../CONTRIBUTING.md), [security model](../SECURITY.md), and
  [privacy statement](../PRIVACY.md).

## Historical records

[`archive/2026-08-development/`](archive/2026-08-development/) preserves the
superseded multipath/erasure notebooks, protocol-3 measurements, profiles,
  candidate reports, fallback measurements, and audits that explain how the
  current design was reached.
They remain useful provenance, but their commands, wire format, conclusions,
and performance numbers are not current release claims.

When current documentation disagrees with an archived record, current
documentation wins. Historical mistakes and negative results remain visible so
future work does not repeat them.
