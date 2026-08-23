`deploy/install-server.sh` and `deploy/install-client.sh`, which perform the
gateway and desktop deployments the guide previously spelled out by hand.
The server script installs the binary, service account, directories, unit,
and environment file, initializes the provider, creates the first user, and
prints one invitation only after the running gateway has been verified. It
refuses to run over an existing provider state unless `--no-provider-init`
says the trust root is being kept, because replacing a root strands every
enrolled device. The client script enrolls one or more invitations, writes
the multi-provider manifest, installs a per-user service that starts at
login -- a LaunchAgent on macOS, a lingering systemd `--user` unit on Linux,
which had no supervisor template at all -- and verifies each SOCKS5 listener
end to end. Re-running it with a new invitation adds a provider and keeps the
existing entries and their ports, and changing `--config-dir`, `--prefix`,
`--label`, or `--service-name` relocates an install rather than starting a
second one beside it: the profiles move with it, because an invitation is
single-use and a profile left behind is a device that cannot be reissued.
