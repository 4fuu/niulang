"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const Parser = require("./parser.js");

test("parses lane traces and derives counter rates", () => {
  const data = Parser.parseText("lane.log", [
    "lane t=1.000 flow=0xabc lane=7 cwnd=400000 inflight=300000 minrtt=190.0ms srtt=205.0ms pacing=1000000 maxbw=900000 floor=0.030 round=8 appsamp=2 nonapp=9 window=262144 held=1200 queued=0 chunks=1 ready=0 flowheld=1200 issued=8 reissued=1 source=8000 breissued=1000 lanefail=0 resid=201ms residmax=240ms acksin=2 acksout=2 acksched=1 ackwr=0ms sent=100000 pktsent=100 lostbytes=3000 lost=3 mode=3",
    "lane t=2.000 flow=0xabc lane=7 cwnd=450000 inflight=350000 minrtt=190.0ms srtt=220.0ms pacing=1200000 maxbw=1000000 floor=0.040 round=9 appsamp=2 nonapp=10 window=262144 held=2000 queued=200 chunks=2 ready=0 flowheld=2200 issued=11 reissued=2 source=12000 breissued=2500 lanefail=0 resid=215ms residmax=280ms acksin=3 acksout=3 acksched=1 ackwr=1ms sent=300000 pktsent=200 lostbytes=8000 lost=8 mode=3",
  ].join("\n"));
  assert.equal(data.points.length, 2);
  assert.equal(data.points[0].metrics.smoothed_rtt_ms, 205);
  assert.equal(data.points[0].metrics.erasure_floor_ratio, 0.03);
  assert.equal(data.points[1].metrics.throughput_up_bps, 1_600_000);
  assert.equal(data.points[1].metrics.loss_packets_per_second, 5);
  assert.equal(data.points[1].metrics.packet_loss_percent, 5);
  assert.equal(data.points[1].metrics.reissued_bytes_per_second, 1500);
});

test("parses structured production lane snapshots", () => {
  const data = Parser.parseText("client.log", [
    JSON.stringify({ time: "2026-08-18T10:00:00Z", msg: "lane performance snapshot", type: "lane_metrics", role: "client", t: 1, flow: "0xabc", lane: 4, cwnd: 400000, inflight: 300000, minrtt: 190, srtt: 205, pacing: 1000000, maxbw: 900000, floor: .03, sent: 100000, lost: 2, mode: 3 }),
    JSON.stringify({ time: "2026-08-18T10:00:01Z", msg: "lane performance snapshot", type: "lane_metrics", role: "client", t: 2, flow: "0xabc", lane: 4, cwnd: 420000, inflight: 310000, minrtt: 190, srtt: 210, pacing: 1100000, maxbw: 950000, floor: .04, sent: 300000, lost: 5, mode: 3 }),
  ].join("\n"));
  assert.equal(data.points.length, 2);
  assert.equal(data.points[1].group, "0xabc / lane 4");
  assert.equal(data.points[1].metrics.throughput_up_bps, 1_600_000);
  assert.equal(data.points[1].metrics.loss_packets_per_second, 3);
});

test("parses JSON slog flow completion and FEC internals", () => {
  const line = JSON.stringify({
    time: "2026-08-18T10:00:00Z", level: "INFO", msg: "local flow complete",
    bytes_up: 1000, bytes_down: 999000, duration: "2s", data_coded: 600,
    data_stream: 200, coded_substrate: "sent=800 repairs=200 recovered=70 lost=2 window=256 coding=true rate=0.75", class: "bulk",
  });
  const data = Parser.parseText("client.jsonl", line);
  assert.equal(data.flows.length, 1);
  assert.equal(data.flows[0].throughput_bps, 4_000_000);
  assert.deepEqual(data.flows[0].fec, {
    sent: 800, repairs: 200, recovered: 70, lost: 2, window: 256, coding: true, rate: 0.75,
  });
});

test("prefers typed production FEC telemetry over the compatibility summary", () => {
  const data = Parser.parseText("server.log", JSON.stringify({
    time: "2026-08-18T10:00:00Z", msg: "remote flow complete", session_id: "abcd", flow_id: 42, transport: "quic", bytes_from_client: 10,
    bytes_to_client: 1000, duration: 1_000_000_000, data_coded: 4, data_stream: 1,
    coded_substrate: "sent=1 repairs=0 recovered=0 lost=0 window=1 coding=false rate=1.00",
    fec_available: true, fec_sent_total: 1200, fec_repairs_total: 400,
    fec_recovered_total: 210, fec_residual_lost_total: 7, fec_window_symbols: 80,
    fec_coding: true, fec_plan_k: 80, fec_plan_n: 120, fec_code_rate: 2 / 3,
    fec_estimated_residual: .001, fec_observed_loss: .31, fec_erasure_floor: .27,
    fec_burst_factor: 1.4, fec_memoryless: false, fec_reason: "sized for the loss floor",
  }));
  assert.equal(data.flows[0].fec.sent, 1200);
  assert.equal(data.flows[0].session_id, "abcd");
  assert.equal(data.flows[0].flow_id, "42");
  assert.equal(data.flows[0].transport, "quic");
  assert.equal(data.flows[0].fec.plan_k, 80);
  assert.equal(data.flows[0].fec.erasure_floor, .27);
  assert.equal(data.flows[0].fec.burst_factor, 1.4);
  assert.equal(data.flows[0].fec.memoryless, false);
  assert.equal(data.flows[0].fec.reason, "sized for the loss floor");
});

test("parses timestamped metrics JSONL and derives goodput and byte loss", () => {
  const data = Parser.parseText("metrics.jsonl", [
    JSON.stringify({ type: "metrics", started_utc: "2026-08-18T10:00:00Z", metrics: {
      queqiao_quic_bytes_received: 1000, queqiao_quic_bytes_sent: 500,
      queqiao_quic_bytes_lost: 10, queqiao_quic_smoothed_rtt_seconds: 0.2,
    }}),
    JSON.stringify({ type: "metrics", started_utc: "2026-08-18T10:00:02Z", metrics: {
      queqiao_quic_bytes_received: 3001000, queqiao_quic_bytes_sent: 1000500,
      queqiao_quic_bytes_lost: 20010, queqiao_quic_smoothed_rtt_seconds: 0.25,
    }}),
  ].join("\n"));
  assert.equal(data.points.length, 2);
  assert.equal(data.points[1].metrics.throughput_down_bps, 12_000_000);
  assert.equal(data.points[1].metrics.throughput_up_bps, 4_000_000);
  assert.equal(data.points[1].metrics.byte_loss_percent, 2);
  assert.equal(data.points[1].metrics.smoothed_rtt_ms, 250);
  assert.equal(data.points[1].group, "metrics.jsonl");
  assert.equal(data.events.length, 0);
});

test("parses production runtime telemetry in JSON and text formats", () => {
  const jsonLog = Parser.parseText("client.log", [
    JSON.stringify({ time: "2026-08-18T10:00:00Z", msg: "performance snapshot", type: "metrics", role: "client", telemetry_schema: 1, queqiao_quic_bytes_received: 1000, queqiao_bytes_down_total: 0, queqiao_quic_packets_sent: 100, queqiao_quic_packets_lost: 2, queqiao_quic_smoothed_rtt_seconds: .2, queqiao_quic_controller_erasure_floor_ratio: .03, queqiao_quic_controller_in_recovery: false }),
    JSON.stringify({ time: "2026-08-18T10:00:05Z", msg: "performance snapshot", type: "metrics", role: "client", telemetry_schema: 1, queqiao_quic_bytes_received: 5001000, queqiao_bytes_down_total: 2500000, queqiao_quic_packets_sent: 1100, queqiao_quic_packets_lost: 52, queqiao_quic_smoothed_rtt_seconds: .25, queqiao_quic_controller_erasure_floor_ratio: .04, queqiao_quic_controller_in_recovery: true }),
  ].join("\n"));
  assert.equal(jsonLog.points.length, 2);
  assert.equal(jsonLog.points[1].group, "client");
  assert.equal(jsonLog.points[1].metrics.throughput_down_bps, 8_000_000);
  assert.equal(jsonLog.points[1].metrics.application_throughput_down_bps, 4_000_000);
  assert.equal(jsonLog.points[1].metrics.packet_loss_percent, 5);
  assert.equal(jsonLog.points[1].metrics.erasure_floor_ratio, .04);
  assert.equal(jsonLog.points[1].metrics.controller_in_recovery, 1);

  const textLog = Parser.parseText("server.log", [
    'time=2026-08-18T10:00:00.000Z level=INFO msg="performance snapshot" service=queqiaod role=server type=metrics telemetry_schema=1 queqiao_quic_bytes_sent=100 queqiao_quic_smoothed_rtt_seconds=0.200 queqiao_quic_controller_in_recovery=false',
    'time=2026-08-18T10:00:05.000Z level=INFO msg="performance snapshot" service=queqiaod role=server type=metrics telemetry_schema=1 queqiao_quic_bytes_sent=500100 queqiao_quic_smoothed_rtt_seconds=0.220 queqiao_quic_controller_in_recovery=true',
  ].join("\n"));
  assert.equal(textLog.points.length, 2);
  assert.equal(textLog.points[1].metrics.throughput_up_bps, 800_000);
  assert.equal(textLog.points[1].metrics.smoothed_rtt_ms, 220);
});

test("keeps runtime lifecycle, configuration, warnings, and errors readable", () => {
  const jsonLog = Parser.parseText("client.log", [
    JSON.stringify({ time: "2026-08-18T10:00:00Z", level: "INFO", msg: "runtime logging initialized", role: "client", log_file: "/tmp/client.log" }),
    JSON.stringify({ time: "2026-08-18T10:00:00Z", level: "INFO", msg: "runtime configuration", role: "client", transport: "auto", congestion: "erasure" }),
    JSON.stringify({ time: "2026-08-18T10:00:01Z", level: "WARN", msg: "transient UDP send failure treated as packet loss", error: "network unreachable" }),
  ].join("\n"));
  assert.equal(jsonLog.events.length, 3);
  assert.equal(jsonLog.events[1].details.congestion, "erasure");
  assert.equal(jsonLog.events[2].status, "failed");

  const textLog = Parser.parseText("server.log", 'time=2026-08-18T10:00:00Z level=INFO msg="remote QUIC listener ready" role=server address=:443');
  assert.equal(textLog.events.length, 1);
  assert.equal(textLog.events[0].status, "ok");
});

test("uses capture labels and recognizes field-soak bundle metadata", () => {
  const capture = Parser.parseText("capture.jsonl", JSON.stringify({
    type: "metrics", label: "client", started_utc: "2026-08-18T10:00:00Z",
    metrics: { queqiao_active_flows: 1 }, status: "ok",
  }));
  assert.equal(capture.points[0].group, "client");

  const summary = Parser.parseText("summary.json", JSON.stringify({
    finished_utc: "2026-08-18T11:00:00Z", udp_attempts: 10, udp_successes: 9,
    https_attempts: 2, https_successes: 2, metrics_delta: {}, passed: false,
  }));
  assert.equal(summary.events[0].status, "failed");
  assert.match(summary.events[0].message, /9\/10 UDP/);

  const manifest = Parser.parseText("manifest.json", JSON.stringify({ label: "opaque", duration_seconds: 60, interval_seconds: 5 }));
  assert.equal(manifest.warnings.length, 0);
  assert.equal(manifest.formats["field-soak manifest"], 1);
});

test("parses benchmark reports and TSV harness output", () => {
  const benchmark = Parser.parseText("bench.json", JSON.stringify({
    schema_version: 1, path: { rtt_ms: 200, loss_percent: 3 },
    trials: [{ stack: "queqiao", flows: 1, trial: 1, mbits_per_sec: 80, complete: true }],
    summary: [{ stack: "queqiao", flows: 1, trials: 1, completed: 1, completion_rate: 1, median_mbits_all_trials: 80 }],
  }));
  assert.equal(benchmark.benchmarks.length, 1);
  assert.equal(benchmark.tabular[0].mbits_per_sec, 80);

  const tsv = Parser.parseText("live.tsv", "round\tlabel\tseconds\tmbits_per_sec\tcomplete\n1\tqueqiao\t2.5\t33.2\t1\n");
  assert.equal(tsv.tabular.length, 1);
  assert.ok(Math.abs(tsv.points[0].metrics.benchmark_throughput_bps - 33_200_000) < 1e-6);
});

test("parses Prometheus text with a scrape timestamp", () => {
  const points = Parser.parsePrometheus([
    "# timestamp: 2026-08-18T10:00:00Z",
    "queqiao_quic_smoothed_rtt_seconds 0.2",
    "queqiao_quic_controller_kind{kind=\"bbr-tuic\"} 1",
  ].join("\n"), "metrics.prom", null);
  assert.equal(points.length, 1);
  assert.equal(points[0].metrics.smoothed_rtt_ms, 200);
  assert.equal(points[0].metrics.queqiao_quic_controller_kind, 1);
});

test("reports unsupported content instead of fabricating records", () => {
  const data = Parser.parseText("unknown.txt", "this is not a transport log");
  assert.equal(data.points.length, 0);
  assert.match(data.warnings[0], /no supported performance records/);
});
