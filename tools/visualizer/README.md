# Queqiao performance visualizer

This is a dependency-free local dashboard for transport profiling. It reads
evidence with the browser File API and does not upload, persist, or serve the
selected files.

## Open it

Open [`index.html`](index.html) directly in a current browser, or serve the
repository if the browser restricts local files:

```sh
python3 -m http.server 8080
# Open http://127.0.0.1:8080/tools/visualizer/
```

Use **Select log files…** for individual logs/reports or **Select evidence
folder…** for a complete field-soak output directory. Multiple client/server
traces can be loaded together and selected with the source and flow/lane
filters. Browser security prevents a local page from scanning the filesystem;
the page can read only the files or directory chosen in the operating-system
picker.

On macOS, click **Select log files…**, press <kbd>Command-Shift-G</kbd> in the
picker, paste `~/Library/Logs/Queqiao/client.log`, and press Return.

The production server file is `0600` and owned by the service account. Keep
that protection in place; make a private user-owned review copy as described
in [`docs/LOGGING.md`](../../docs/LOGGING.md) before selecting it in a desktop
browser.

## Where the files are

Both `queqiaod client` and `queqiaod server` create a bounded JSON runtime log
automatically. Run `queqiaod logs` to print the absolute paths, current sizes,
and follow commands. The standard locations are:

| Run method | Location to select |
| --- | --- |
| macOS client | `~/Library/Logs/Queqiao/client.log` |
| macOS interactive server | `~/Library/Logs/Queqiao/server.log` |
| Linux user process | `${XDG_STATE_HOME:-~/.local/state}/queqiao/client.log` or `server.log` |
| production systemd server | `/var/log/queqiao/server.log` |
| explicit `--log-file PATH` | the exact `PATH`; it is also recorded in the startup entry |
| `queqiaobench --json PATH` | the exact `PATH`, commonly `/tmp/bench.json` |
| `field_soak.py --output-dir DIR` | select the entire `DIR` with **Select evidence folder…** |

The active log rotates to `.1` through `.5` at 32 MiB. Select multiple active
and rotated files together to inspect a longer timeline. See
[`docs/LOGGING.md`](../../docs/LOGGING.md) for all controls and platform paths.

## Capture useful timelines

The runtime log already contains a performance snapshot every five seconds
while a flow is active or counters change. For one-second aggregate and raw
per-lane profiling, start either role with:

```sh
QUEQIAO_LANE_TRACE=1 ./queqiaod client \
  --profile "$PROFILE" --telemetry-log-interval 1s

queqiaod logs client
```

Load the printed `client.log` path. Aggregate records supply the standard time
series; the opt-in lane records add raw congestion and scheduler state; flow
completion records supply FEC outcomes and the coded/stream payload split.

`scripts/capture_metrics.py` remains available when an independently scraped
`/metrics` record is needed. The metrics listener is intentionally not enabled
by default and should remain on loopback.

For benchmark comparisons, retain the machine-readable report:

```sh
go run ./cmd/queqiaobench --rtt 200 --loss 3 --rate 100 \
  --trials 5 --interactive --json /tmp/bench.json
```

## Accepted evidence

| Input | What the dashboard extracts |
| --- | --- |
| default client/server runtime log | timestamped aggregate RTT, goodput, loss, controller, lane, fallback, timeout, recovery, and flow state plus lifecycle/errors |
| `QUEQIAO_LANE_TRACE=1` runtime records | per-lane RTT, pacing and delivery estimate, congestion window, in-flight data, erasure floor, scheduler occupancy/residency, ACK work, retransmission, loss, controller mode |
| client/server runtime log | flow duration, bytes, derived flow throughput, class, coded/stream payload split, and typed FEC sent/repair/recovered/residual/window/rate/estimator fields |
| `capture_metrics.py` JSONL | timestamped aggregate RTT, byte-counter goodput, byte loss, controller state, lanes, active flows, fallbacks, reinjections, and other exported metrics |
| Prometheus text | one or more snapshots; use `# timestamp: ISO-8601` between concatenated scrapes to retain time |
| `queqiaobench --json` | path conditions, per-trial goodput/completion, summary cells, cold/warm and interactive latency |
| `field_soak.py` evidence | UDP/HTTPS probe latency and failures, periodic metrics and process snapshots |
| Queqiao harness TSV/CSV | goodput, duration, completion, and loss columns when present |

The parser uses names and field shapes, not filenames, so renamed archival
artifacts still work. Unsupported files are reported in the import warnings.

## Reading derived values

- **Goodput** from metrics is the delta of the cumulative sent/received byte
  counter divided by the elapsed time. It is available only with at least two
  samples from the same source and group.
- **Byte loss percent** is the interval delta of QUIC bytes lost divided by
  the interval delta of QUIC bytes sent. It is not packet loss probability;
  the current metric surface does not export a sent-packet denominator.
- **Packet loss activity** is the derivative of the cumulative packets-lost
  counter, expressed as lost packets per second.
- **FEC recovery effectiveness** is `recovered / (recovered + residual lost)`.
  Repair share is `repair datagrams / sent datagrams`. The headline cards use
  the latest selected flow record because a pooled coded substrate can repeat
  cumulative counters at multiple flow completions. The table retains every
  observation for comparison without incorrectly summing those snapshots.
- Diagnostic cards are deliberately heuristic. They identify correlations to
  inspect in the underlying charts; they do not replace benchmark gates or
  correctness tests.

## Development checks

```sh
node --test tools/visualizer/parser.test.js
python3 -m unittest scripts.test_capture_metrics
```

The UI has no third-party runtime dependency. `parser.js` remains a classic
script rather than an ES module so opening `index.html` through `file://` works
without a local web server.
