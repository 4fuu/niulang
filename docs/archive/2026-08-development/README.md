# August 2026 development archive

> [!WARNING]
> These records are historical. Most transport and release reports describe
> wire protocol 3 or an earlier design; none qualifies the current protocol-1
> tree for release. Use the [current documentation index](../../README.md) for
> operational guidance.

## Design evolution

- `DESIGN-MULTIPATH.md` — the measured multipath hypothesis and why it was
  retired.
- `DESIGN-ERASURE.md` — the development record that led to the current
  erasure-channel design.
- `PERFORMANCE-20260812.md` and `STALL-20260817.md` — performance recovery,
  causal debugging, rejected changes, and corrections.

## Measurements and profiles

- `MEASUREMENTS-20260809.md`, `MEASUREMENTS-20260810.md`, and
  `MEASUREMENTS-20260816.md`
- `PROFILE-20260811.md`

Some early results were invalid because the supposedly direct route was
captured by an existing TUN. The current
[path characterization](../../PATH-CHARACTER-20260813.md) states that boundary
explicitly.

## Release and audit records

- `RELEASE-HARDENING-20260817.md`
- `RELEASE-CANDIDATE-20260817.md`
- `TCP-FALLBACK-20260817.md`
- `STATIC-SECURITY-AUDIT-20260817.md`
- `PUBLIC-HISTORY-AUDIT-20260817.md`
- `field-results/20260817-primary-high-port.md`

The candidate and field result identify wire protocol 3. They are preserved as
engineering provenance and do not satisfy current protocol-1 release gates.
