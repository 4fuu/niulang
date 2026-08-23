The client's default SOCKS5 listener moved from `127.0.0.1:1080` to
`127.0.0.1:12080`, the port `deploy/clash-queqiao.yaml` and the deployment
guide already point Clash at. The old default disagreed with the profile
shipped beside it, so an unconfigured start routed nowhere. Deployments that
pass `--listen` explicitly are unaffected.
