# Niulang (牛郎)

Niulang is an independent transport project originally forked from
[Queqiao](https://github.com/bojieli/queqiao). Both projects have since evolved
substantially, so Niulang now follows its own design and release path.

The goal is useful bandwidth with the lowest practical latency and packet loss.
Niulang pursues that goal through path-aware congestion and erasure control,
adaptive FEC, shared wire pacing, warm connection reuse, and extensive seeded
testing rather than one fixed strategy for every network.

Protocol 3 uses QUIC/UDP exclusively. Data runs over real HTTP/3 Extended
CONNECT, while enrollment and renewal use dedicated QUIC ALPNs on the same UDP
endpoint. It has no outer TCP fallback or compatibility path to earlier state,
profiles, or wire versions. See the [protocol](docs/PROTOCOL.md), [deployment
guide](docs/DEPLOYING.md), and [security model](SECURITY.md).

Niulang supports Linux, macOS, and Windows on amd64 and arm64. Build the main
binary with Go 1.25.13:

```sh
go build ./cmd/niulangd
```
