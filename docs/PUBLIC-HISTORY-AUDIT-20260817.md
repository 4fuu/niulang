# Public-history baseline audit — 2026-08-17

## Scope and result

The pre-release repository history was scanned before adding the public-release
readiness changes. Gitleaks 8.24.3 examined every commit reachable from local
refs and reported zero findings across 251 scanned commits and approximately
2.21 MB of patch content. `git rev-list --count --all` reported 260 reachable
commits; the scanner count excludes reachable commits with no scannable patch
content.

The scanner archive was downloaded from the official Gitleaks v8.24.3 release
and verified against its published checksum. The Darwin arm64 archive used for
the baseline had SHA-256:

```text
b90f13bb8c90ab72083d9b0c842e39dafb82c0e5c3f872f407366b7a58909013
```

A generated high-entropy GitHub-token canary produced the expected finding exit
before the repository scan. The canary was invalid, existed only in a temporary
directory outside the repository, was not printed, and was deleted afterward.

## Reproduction

The checked-in `scripts/scan_history_secrets.sh` repeats the checksum-verified
download, canary, and full-history scan. For the final candidate:

```sh
QUEQIAO_SECRET_SCAN_REPORT=history-secret-scan.json \
  ./scripts/scan_history_secrets.sh .
```

The baseline does not waive a candidate scan: every new commit changes the
history under review. The final zero-finding JSON and workflow run must be
attached to the release-candidate evidence.

## Operational identifiers

The scan detects credentials, not privacy-sensitive infrastructure metadata.
The current tree was therefore reviewed separately for active addresses,
private hostnames, and machine-specific paths. The live egress address and
local/server rollback paths in the hardening report were replaced with explicit
placeholders. Historical measurement prose now also uses semantic placeholders
for the former egress/SNI and subscriber, LAN, and tether addresses. Public DNS
resolver addresses, RFC 5737 documentation ranges, reserved Clash fake-IP
ranges, example ports, and loopback addresses remain because they are protocol
or deployment documentation rather than private identifiers.

No history rewrite was performed: no credential or private key was found, and
rewriting signed collaboration history solely to remove an already-routable
measurement address would destroy provenance without revoking that address.
Field credentials and TLS material are rotated independently before the final
candidate is approved.
