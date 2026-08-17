# Known limitations and supported scope

## Release classification

The planned v0.1 release is an experimental public preview, not a general VPN,
an anonymity system, or a production-readiness certification. Its supported
topology is one operator-controlled local agent using one fixed egress agent
across a high-latency or lossy path.

## Supported integration

- Clash Verge or another mihomo-based client owns transparent TUN capture and
  forwards selected TCP and UDP traffic to queqiao's loopback SOCKS5 listener.
- Linux, macOS, and Windows archives are produced for amd64 and arm64. A build
  target is not considered runtime-qualified until its downloaded archive has
  passed the native smoke gate recorded for the release candidate.
- GitHub currently labels its standard Linux arm64 and Windows arm64 hosted
  runners as public preview. If either runner is unavailable for the candidate,
  that archive remains cross-built but unqualified and blocks release; a
  successful build alone must not be presented as native validation. See the
  [GitHub-hosted runners reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
- The egress listens on the same TCP and UDP port and must be directly
  reachable. Port 443 is the most broadly traversable choice where the host can
  dedicate it to queqiao.
- TLS server authentication is mandatory. The local agent and egress share one
  high-entropy session secret stored outside release directories.

Direct in-process TUN or VLESS ingress, browser extensions, mobile clients,
automatic egress provisioning, multi-egress selection, and load balancing are
outside v0.1.

## Security and tenancy

- One pre-shared secret identifies the whole client side. There are no
  per-user identities, quotas, revocation lists, or online secret rotation.
  This is acceptable only for the paired single-operator topology.
- SOCKS5 ingress has no user authentication. Bind it to loopback or an
  access-controlled interface.
- Metrics have no authentication and expose operational metadata. Bind them to
  loopback or leave them disabled.
- Secrets are not locked in memory. Restrict configuration permissions and
  disable core dumps for the service.
- `--allow-private-destinations` disables the normal egress SSRF boundary and
  is not supported for an Internet-facing public deployment.

## Protocol and recovery

- v0.1 speaks wire protocol 3 only. A mismatched peer fails immediately; there
  is no downgrade to version 2. A wire-version change requires a coordinated
  endpoint upgrade unless a future release documents a transition.
- TCP recovery preserves ordered bytes and rejects duplicates. UDP recovery
  preserves the association's remote relay address when its short-lived resume
  token succeeds, but datagrams in flight during failure detection are lost as
  normal UDP semantics permit.
- Automatic fallback addresses UDP blocking or path failure. It does not make
  every restrictive network usable: a network may block the chosen port,
  server name, certificate path, or both transports.
- Resource use is bounded per flow, but the host-wide ceiling is the configured
  session limit multiplied by per-flow bounds. The default is not a sizing
  guarantee.

## Qualification status

Repository-controlled correctness, deterministic impairment, fallback, race,
fuzz, vulnerability, packaging, rollback, and bounded live-soak gates are
implemented. Wider independent NAT/middlebox field validation and an external
security review are required before describing the project as production
ready. Their exact acceptance criteria are in `FIELD-VALIDATION.md` and
`SECURITY-ASSESSMENT.md`.
