# Security policy

## Supported versions

Queqiao has not published its first release. Until `v0.1.0` is tagged, only
the current `main` branch receives security fixes. After publication, the
latest `v0.1.x` patch release will be supported; older pre-1.0 builds may be
asked to upgrade before a report is investigated.

| Version | Supported |
| --- | --- |
| `main` before v0.1.0 | Yes |
| Latest `v0.1.x` | Yes, after publication |
| Older builds | No |

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's
[private vulnerability reporting](https://github.com/bojieli/queqiao/security/advisories/new)
to send the affected version or commit, deployment mode, reproduction steps,
impact, and any suggested mitigation. Remove credentials, private keys,
session secrets, packet captures containing user traffic, and private host
information before attaching material.

If private vulnerability reporting is unavailable, contact the maintainer
through the GitHub profile without vulnerability details and request a private
channel.

The maintainer will acknowledge a report as soon as practical, reproduce and
classify it, coordinate a fix and disclosure date, and credit the reporter if
requested. This is a best-effort open-source process, not a response-time SLA.

## Security boundary

The supported v0.1 topology is one operator-controlled local SOCKS5 agent and
one fixed egress agent. The local SOCKS listener and metrics listener have no
authentication and must remain on loopback or another access-controlled
interface. A shared secret authenticates the whole client side; v0.1 does not
provide per-user identity, quota, revocation, or online key rotation.

The detailed threat model, implemented limits, and accepted residual risks are
in [`docs/SECURITY-REVIEW.md`](docs/SECURITY-REVIEW.md). Enabling
`--allow-private-destinations` deliberately removes the egress SSRF boundary
and is outside the supported public deployment profile.

## Disclosure and release handling

Security fixes are prepared privately when disclosure would create immediate
risk. Release artifacts must pass the normal and deep verification described
in [`docs/RELEASE-CHECKLIST.md`](docs/RELEASE-CHECKLIST.md). Credentials used
for development, field validation, and production are never release inputs.
