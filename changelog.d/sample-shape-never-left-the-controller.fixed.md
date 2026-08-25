The bandwidth sample shape is published rather than reading zero.
`queqiao_quic_sample_mean_bytes_per_second`, `..._max_bytes_per_second`,
`..._max_delivered_bytes` and `..._max_interval_seconds` were added in v0.3.0
to describe the delivery-rate samples the bandwidth estimate is built from.
The controller stored all four in atomics and the snapshot that carries
telemetry out of it never read them back, so every one of them left the
process as zero.

That is the failure mode v0.3.0 removed two other counters for: a figure that
cannot be produced reads as a measurement rather than as an absence, and
"the widest sample this connection ever took was nothing" is a claim these
metrics were making on every scrape. It was found by deploying v0.3.0 to the
gateway it was written for and reading the metric it added.

The four are the instrumentation for a question that a harness could not
settle -- an estimator driven against a simulated policer reports within one
per cent of the path while the same estimator in the full stack reported
twice it -- so publishing them as zero left that question exactly as open as
it was before they existed.
