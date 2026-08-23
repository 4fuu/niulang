The provider unit now grants `CAP_NET_BIND_SERVICE`. `deploy/queqiaod.service`
runs as an unprivileged account with `NoNewPrivileges=true`, so the `--listen
:443` the guide recommends could not bind: the capability has to be granted
at exec because nothing may acquire it afterwards. The capability bounding
set is now that one capability rather than the full set.
