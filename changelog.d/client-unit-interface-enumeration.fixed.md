The installed Linux client service can resolve `--local-address` again. The
generated systemd user unit allowed only `AF_INET`, `AF_INET6`, and
`AF_UNIX`, but reading this machine's interfaces needs `AF_NETLINK`:
`if:NAME` looks up the named interface's address, and `auto` has to know
whether the choice is ambiguous. Without it the unit installed, the SOCKS5
listener bound, systemd reported the service active, and every flow then
failed with `enumerate local interfaces: netlinkrib: address family not
supported by protocol`. The option this broke is the one the installer
recommends when `auto` cannot decide between two uplinks.
