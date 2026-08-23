`deploy/install-server.sh` waits for the gateway's listener instead of
looking for it once. `Type=simple` reports a service active as soon as its
process is forked, so the check ran before the socket was bound and a
successful upgrade of a live gateway exited non-zero claiming nothing was
listening on a port that was serving traffic seconds later.
