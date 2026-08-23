# Runtime logging

Both `queqiaod client` and `queqiaod server` create a structured runtime log
by default. The log is the durable operational record; stderr is a second copy
for an interactive terminal or service journal.

## Find the active log

```sh
queqiaod logs
queqiaod logs client
queqiaod logs server
```

The command prints the absolute default path, whether it exists, its current
size, and a command that follows it. The default files are separate:

| Platform | Client | Server when run interactively |
| --- | --- | --- |
| macOS | `~/Library/Logs/Queqiao/client.log` | `~/Library/Logs/Queqiao/server.log` |
| Linux | `${XDG_STATE_HOME:-~/.local/state}/queqiao/client.log` | `${XDG_STATE_HOME:-~/.local/state}/queqiao/server.log` |
| Windows | `%LOCALAPPDATA%\Queqiao\Logs\client.log` | `%LOCALAPPDATA%\Queqiao\Logs\server.log` |

The production systemd unit sets `QUEQIAO_LOG_DIR=/var/log/queqiao`, so its
server log is `/var/log/queqiao/server.log`. The macOS LaunchAgent template
sets the client file explicitly to the macOS path above.

An explicit `--log-file` always wins. Relative paths are converted to absolute
paths at startup, `~/` is expanded, and the resolved path is recorded in the
first `runtime logging initialized` entry. The parent directory is created if
needed.

Read the current and rotated files with ordinary tools:

```sh
tail -n 200 -f ~/Library/Logs/Queqiao/client.log
tail -n 200 /var/log/queqiao/server.log
ls -lh /var/log/queqiao/server.log*
```

The visualizer's **Select log files…** action accepts the active file and any
rotated `.1`, `.2`, and later files together.

The system service owns its `0600` server log, so a desktop browser normally
cannot open it directly. Copy only the evidence you need into your current
user's private directory; do not make the service log world-readable:

```sh
mkdir -m 0700 ./queqiao-log-review
sudo install -m 0600 -o "$(id -u)" -g "$(id -g)" \
  /var/log/queqiao/server.log ./queqiao-log-review/server.log
```

Then choose `queqiao-log-review/server.log` in the visualizer and remove the
review copy when the investigation is finished.

## Defaults and controls

The production defaults are:

- JSON Lines format, one complete object per line;
- level `info`;
- a 32 MiB active file;
- five rotated backups (`client.log.1` through `client.log.5`, likewise for
  the server);
- mode `0600` for active and newly rotated files;
- a performance snapshot every five seconds while flows are active or state
  changes; and
- a second copy on stderr for a terminal or the service journal.

Both client and server accept the same flags:

```text
--log-file PATH                 auto, an explicit path, or none
--log-format json|text          json by default
--log-level debug|info|warn|error
--log-stderr=true|false         mirror to stderr/service journal
--log-max-size-mib 32
--log-max-backups 5
--telemetry-log-interval 5s     0 disables periodic snapshots
```

`--json-logs` remains as a deprecated compatibility alias for
`--log-format=json`. `--log-file=none` is an explicit container/console mode
and is rejected unless stderr logging remains enabled.

For finer profiling, use `--telemetry-log-interval 1s`. Values below one
second are rejected to prevent an accidental log flood. A level above `info`
also suppresses performance snapshots, so `info` or `debug` is required for
dashboard time series.

## What a runtime log contains

Every entry includes `time`, `level`, `msg`, `service`, `role`, and `pid`.
Startup records also identify the build, wire protocol, resolved log path,
format, retention, telemetry interval, and a non-secret snapshot of transport,
congestion, framing, timeout, pooling, and admission settings. Shutdown
failures are written to the file before it is closed.

Performance records use `msg="performance snapshot"`, `type="metrics"`, and
`telemetry_schema=1`. Their flat `queqiao_*` fields intentionally match the
Prometheus `/metrics` names. They cover:

- active/started/completed/failed flows and transferred bytes;
- latest, smoothed, and controller-minimum RTT;
- QUIC sent, received, and lost bytes plus sent, received, and lost packets;
- delivery, ACK, send, pacing, and maximum-bandwidth estimates;
- congestion window, bytes in flight, controller round/mode/recovery;
- lanes, failures, replacements, reinjections, fallbacks, and timeouts;
- transient local UDP send errors absorbed into QUIC loss recovery;
- flow telemetry entries expired because nothing refreshed them, which is how
  a round-trip aggregate frozen at a stale constant announces itself; and
- the controller's measured erasure floor, sampler diagnostics, and class
  transitions.

The dashboard calculates interval packet loss from changes in sent and lost
packet counters. QUIC can later recognize a packet previously declared lost,
so its lost byte/packet counters are allowed to decrease; the dashboard skips
that interval instead of displaying a fabricated negative loss rate.

Flow-completion records add an opaque session/flow correlation ID, transport,
duration, directional bytes, class, lane byte
allocation, coded-versus-stream payload, and the FEC sent/repair/recovered/
residual/window/rate summary.

The FEC counters have two directions and they must not be divided into each
other. `fec_sent_total` and `fec_repairs_total` are what this endpoint
transmitted; `fec_arrived_total`, `fec_recovered_total` and
`fec_residual_lost_total` are what it received, so on an asymmetric flow
`lost` above `sent` is ordinary rather than impossible. The receive direction's
rates are `fec_measured_erasure`, the share of the peer's source symbols that
did not arrive, and `fec_residual_loss`, the share the code could not repair
and the session had to re-issue. Both are taken over `fec_source_symbols_total`
and are therefore in [0,1]. Failed flows are warning-level records with the
same performance and FEC fields plus the error, so they remain visible at the
default `info` level. `QUEQIAO_LANE_TRACE=1` remains an opt-in raw
per-lane diagnostic. It is not needed for the standard aggregate dashboard.

No application payload is logged. Operational logs can contain configured
endpoint addresses and error text; debug records may also contain local uplink
addresses and device/account identifiers. Treat the `0600` files as sensitive
operational data.

## Service operation

The production systemd service writes the file and mirrors JSON records to
journald. Either surface can diagnose startup when the other is unavailable:

```sh
sudo tail -f /var/log/queqiao/server.log
sudo journalctl -u queqiaod -f
```

Rotation is internal and does not require `logrotate`, a SIGHUP, or reopening
the process. Do not configure an external rotator to rename the same files.
The process fails startup rather than silently running without its configured
file when the directory cannot be created or the file cannot be opened.
