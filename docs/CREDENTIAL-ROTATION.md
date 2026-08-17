# Credential and certificate rotation

## Scope

v0.1 authenticates the paired client with one pre-shared secret and authenticates
the egress with a private TLS root. It does not support overlapping secrets or
online trust-root rollover, so rotating either requires a coordinated brief
maintenance window. A failed rotation must restore the complete old set; never
mix an old root with a new leaf or different client/server secrets.

## Generate a replacement set

Run this on a trusted workstation, outside the repository:

```sh
./scripts/generate_credentials.sh \
  --output /secure/new-queqiao-credentials \
  --server-name queqiao.node
```

The script creates a P-256 private root, a 397-day server leaf with only the
requested DNS SAN and server-auth usage, and a printable 384-bit session
secret. It verifies the chain and name before returning. Permissions are 0600
for private keys/secret and 0644 for certificates.

- Keep `root-ca.key` offline; it is not a service input.
- Deploy `server.crt` and `server.key` only to the egress.
- Deploy `root-ca.crt` and `secret` to the client.
- Deploy `secret` to the egress through an encrypted authenticated channel.
- Never copy the root private key, packet captures, or prior secrets into an
  evidence bundle or release directory.

## Coordinated replacement

1. Record current binary versions, certificate fingerprints/expiry, file
   owners/modes, service health, HTTPS egress, UDP association behavior, and
   exact rollback paths. Back up all five deployed copies (client root/secret
   and egress leaf/key/secret) in a mode-0700 directory outside the release
   tree; private members remain mode 0600.
2. Verify the new leaf locally with `openssl verify` and `openssl x509
   -checkhost`. Compare the client/server secret locally without printing it,
   for example by transferring it once and using `cmp` over the authenticated
   administrative channel.
3. Stop the local agent so it cannot repeatedly authenticate with the old set.
4. Stop the egress, atomically install its new leaf/key and secret with the
   original owner and restrictive modes, then start it and verify its TCP and
   UDP listeners.
5. Atomically install the new client root and secret, then start the client.
6. Run verified HTTPS plus a persistent SOCKS5 UDP check. Confirm TLS presents
   the expected new leaf, QUIC is preferred, no authentication warnings appear,
   and metrics return to zero active flows after the probes.
7. Retain the old set only for a bounded rollback period. Once the new set is
   accepted, securely remove the old secret and private key from live and
   rollback directories. A normal binary rollback does not require credential
   rollback because credentials live outside release directories.

If any verification fails, stop both agents, restore all backed-up credential
files atomically, restore their modes/ownership, restart the egress then client,
and repeat the HTTPS/UDP checks. Do not troubleshoot by disabling certificate
verification or by enabling private destinations.

## Routine interval and compromise

Rotate the server leaf before expiry and rotate the shared secret at least with
each operator/device change. Rotate the root on a longer schedule or immediately
if its private key may be exposed. Suspected shared-secret or server-key exposure
requires immediate coordinated rotation and invalidation/removal of every old
copy; deleting a Git commit alone does not revoke a copied credential.
