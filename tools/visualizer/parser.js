/*
 * Queqiao performance-log parser.
 *
 * This file deliberately has no dependencies and works as both a classic
 * browser script and a CommonJS module, so the dashboard works from file://
 * while the parsing logic remains unit-testable with Node.
 */
(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  root.QueqiaoLogParser = api;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  const numberPattern = "[-+]?(?:\\d+(?:\\.\\d*)?|\\.\\d+)(?:[eE][-+]?\\d+)?";
  const durationUnits = { ns: 1e-6, us: 1e-3, "µs": 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };
  const laneAliases = {
    cwnd: "cwnd_bytes",
    inflight: "inflight_bytes",
    minrtt: "min_rtt_ms",
    srtt: "smoothed_rtt_ms",
    pacing: "pacing_bytes_per_second",
    maxbw: "max_bandwidth_bytes_per_second",
    floor: "erasure_floor_ratio",
    round: "controller_round",
    appsamp: "app_limited_samples_total",
    nonapp: "non_app_limited_samples_total",
    window: "scheduler_window_bytes",
    held: "lane_held_bytes",
    queued: "lane_queued_bytes",
    chunks: "lane_outstanding_chunks",
    ready: "ready_chunks",
    flowheld: "flow_held_bytes",
    issued: "chunks_issued_total",
    reissued: "chunks_reissued_total",
    source: "source_bytes_total",
    breissued: "bytes_reissued_total",
    lanefail: "lane_failures_total",
    resid: "mean_residency_ms",
    residmax: "max_residency_ms",
    acksin: "acks_in",
    acksout: "acks_out",
    acksched: "acks_scheduled",
    ackwr: "ack_write_ms",
    sent: "quic_bytes_sent_total",
    pktsent: "quic_packets_sent_total",
    pktrecv: "quic_packets_received_total",
    lostbytes: "quic_bytes_lost_total",
    lost: "quic_packets_lost_total",
    mode: "controller_mode",
  };

  function emptyDataset() {
    return {
      points: [], benchmarks: [], flows: [], events: [], tabular: [],
      sources: [], warnings: [], formats: {},
    };
  }

  function addSource(data, name, format, count) {
    if (!data.sources.includes(name)) data.sources.push(name);
    data.formats[format] = (data.formats[format] || 0) + (count || 1);
  }

  function finiteNumber(value) {
    if (typeof value === "number") return Number.isFinite(value) ? value : null;
    if (typeof value !== "string" || !value.trim()) return null;
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }

  function parseTime(value) {
    if (value === null || value === undefined || value === "") return null;
    if (typeof value === "number") {
      if (!Number.isFinite(value)) return null;
      if (value > 1e15) return value / 1e6;
      if (value > 1e12) return value;
      if (value > 1e9) return value * 1000;
      return null;
    }
    const numeric = Number(value);
    if (Number.isFinite(numeric) && String(value).trim() !== "") return parseTime(numeric);
    const parsed = Date.parse(value);
    return Number.isFinite(parsed) ? parsed : null;
  }

  function parseDurationMillis(value) {
    if (typeof value === "number") return Number.isFinite(value) ? value / 1e6 : null;
    if (typeof value !== "string") return null;
    let total = 0;
    let matched = false;
    const expression = new RegExp(`(${numberPattern})\\s*(ns|us|µs|ms|s|m|h)`, "g");
    for (const match of value.matchAll(expression)) {
      total += Number(match[1]) * durationUnits[match[2]];
      matched = true;
    }
    return matched ? total : finiteNumber(value);
  }

  function unquote(value) {
    if (!value || value[0] !== '"') return value;
    try { return JSON.parse(value); } catch (_) { return value.slice(1, -1); }
  }

  function parseLogfmt(line) {
    const result = {};
    const expression = /([A-Za-z_][\w.-]*)=("(?:\\.|[^"\\])*"|[^\s]+)/g;
    for (const match of line.matchAll(expression)) result[match[1]] = unquote(match[2]);
    return result;
  }

  function parseCodedSubstrate(value) {
    if (!value || value === "none") return null;
    const fields = parseLogfmt(value);
    const result = {};
    for (const key of ["sent", "repairs", "recovered", "lost", "window"]) {
      const number = finiteNumber(fields[key]);
      if (number !== null) result[key] = number;
    }
    if (fields.coding !== undefined) result.coding = fields.coding === true || fields.coding === "true";
    const rate = finiteNumber(fields.rate);
    if (rate !== null) result.rate = rate;
    return Object.keys(result).length ? result : null;
  }

  function booleanValue(value) {
    if (value === true || value === "true") return true;
    if (value === false || value === "false") return false;
    return null;
  }

  function parseStructuredFEC(record) {
    const available = booleanValue(record.fec_available);
    if (available === false) return null;
    if (available !== true && record.fec_sent_total === undefined) return null;
    const mapping = {
      sent: "fec_sent_total", repairs: "fec_repairs_total", recovered: "fec_recovered_total",
      lost: "fec_residual_lost_total", oversize: "fec_oversize_total", window: "fec_window_symbols",
      plan_k: "fec_plan_k", plan_n: "fec_plan_n", rate: "fec_code_rate",
      estimated_residual: "fec_estimated_residual", loss_coded: "fec_loss_coded",
      effective_burst: "fec_effective_burst", observed_samples: "fec_observed_samples",
      observed_loss: "fec_observed_loss", loss_after_arrival: "fec_loss_after_arrival",
      arrival_after_loss: "fec_arrival_after_loss", mean_burst: "fec_mean_burst",
      burst_factor: "fec_burst_factor", erasure_floor: "fec_erasure_floor",
      congestive_loss: "fec_congestive_loss", recent_loss: "fec_recent_loss",
      reordered: "fec_reordered_total", decided: "fec_decided_total",
    };
    const result = {};
    for (const [name, field] of Object.entries(mapping)) {
      const number = finiteNumber(record[field]);
      if (number !== null) result[name] = number;
    }
    const coding = booleanValue(record.fec_coding);
    const memoryless = booleanValue(record.fec_memoryless);
    if (coding !== null) result.coding = coding;
    if (memoryless !== null) result.memoryless = memoryless;
    if (record.fec_reason !== undefined) result.reason = String(record.fec_reason);
    return result;
  }

  function valueFrom(record, names) {
    for (const name of names) if (record[name] !== undefined) return record[name];
    return undefined;
  }

  function parseFlowRecord(record, source) {
    const message = String(valueFrom(record, ["msg", "message"]) || "");
    if (!/flow (?:complete|ended with error)/i.test(message)) return null;
    const durationMs = parseDurationMillis(record.duration);
    const up = finiteNumber(valueFrom(record, ["bytes_up", "bytes_from_client"])) || 0;
    const down = finiteNumber(valueFrom(record, ["bytes_down", "bytes_to_client"])) || 0;
    const flow = {
      source,
      time: parseTime(valueFrom(record, ["time", "timestamp", "ts", "started_utc"])),
      endpoint: /^remote/.test(message) ? "remote" : "local",
      status: /error/i.test(message) ? "failed" : "complete",
      duration_ms: durationMs,
      bytes_up: up,
      bytes_down: down,
      data_coded: finiteNumber(record.data_coded) || 0,
      data_stream: finiteNumber(record.data_stream) || 0,
      session_id: record.session_id === undefined ? "" : String(record.session_id),
      flow_id: record.flow_id === undefined ? "" : String(record.flow_id),
      transport: record.transport === undefined ? "" : String(record.transport),
      class: record.class === undefined ? "" : String(record.class),
      fec: parseStructuredFEC(record) || parseCodedSubstrate(record.coded_substrate),
      message,
    };
    if (durationMs > 0) flow.throughput_bps = (up + down) * 8 / (durationMs / 1000);
    return flow;
  }

  function normalizePrometheus(metrics) {
    const normalized = {};
    const copy = (from, to, factor) => {
      if (metrics[from] !== undefined) normalized[to] = metrics[from] * (factor || 1);
    };
    copy("queqiao_quic_latest_rtt_seconds", "latest_rtt_ms", 1000);
    copy("queqiao_quic_smoothed_rtt_seconds", "smoothed_rtt_ms", 1000);
    copy("queqiao_quic_controller_min_rtt_seconds", "min_rtt_ms", 1000);
    copy("queqiao_quic_controller_max_bandwidth_bytes_per_second", "max_bandwidth_bytes_per_second");
    copy("queqiao_quic_controller_latest_sample_bytes_per_second", "delivery_sample_bytes_per_second");
    copy("queqiao_quic_controller_latest_ack_rate_bytes_per_second", "ack_rate_bytes_per_second");
    copy("queqiao_quic_controller_latest_send_rate_bytes_per_second", "send_rate_bytes_per_second");
    copy("queqiao_quic_controller_pacing_rate_bytes_per_second", "pacing_bytes_per_second");
    copy("queqiao_quic_controller_congestion_window_bytes", "cwnd_bytes");
    copy("queqiao_quic_controller_bytes_in_flight", "inflight_bytes");
    copy("queqiao_quic_controller_bytes_lost", "controller_bytes_lost_total");
    copy("queqiao_quic_controller_packets_lost", "controller_packets_lost_total");
    copy("queqiao_quic_controller_erasure_floor_ratio", "erasure_floor_ratio");
    copy("queqiao_quic_bytes_sent", "quic_bytes_sent_total");
    copy("queqiao_quic_bytes_received", "quic_bytes_received_total");
    copy("queqiao_quic_bytes_lost", "quic_bytes_lost_total");
    copy("queqiao_quic_packets_sent", "quic_packets_sent_total");
    copy("queqiao_quic_packets_received", "quic_packets_received_total");
    copy("queqiao_quic_packets_lost", "quic_packets_lost_total");
    copy("queqiao_quic_controller_in_recovery", "controller_in_recovery");
    copy("queqiao_quic_controller_mode", "controller_mode");
    copy("queqiao_active_flows", "active_flows");
    copy("queqiao_quic_lanes", "quic_lanes");
    copy("queqiao_bytes_up_total", "application_bytes_up_total");
    copy("queqiao_bytes_down_total", "application_bytes_down_total");
    copy("queqiao_udp_transient_send_errors_total", "transient_udp_send_errors_total");
    return Object.assign({}, metrics, normalized);
  }

  function metricNameWithoutLabels(name) {
    return name.replace(/\{.*\}$/, "");
  }

  function parsePrometheus(text, source, defaultTime) {
    const points = [];
    let metrics = {};
    let blockTime = defaultTime || null;
    const flush = () => {
      if (!Object.keys(metrics).length) return;
      points.push({ source, kind: "metrics", time: blockTime, elapsed: null, group: source, metrics: normalizePrometheus(metrics) });
      metrics = {};
      blockTime = null;
    };
    for (const raw of text.split(/\r?\n/)) {
      const line = raw.trim();
      if (!line) {
        if (Object.keys(metrics).length && blockTime !== null) flush();
        continue;
      }
      const timeComment = line.match(/^#\s*(?:timestamp|time|scrape(?:_time)?)\s*[:= ]\s*(.+)$/i);
      if (timeComment) {
        if (Object.keys(metrics).length) flush();
        blockTime = parseTime(timeComment[1].trim());
        continue;
      }
      if (line[0] === "#") continue;
      const match = line.match(new RegExp(`^(queqiao_[^\\s]+)\\s+(${numberPattern})(?:\\s+(${numberPattern}))?$`));
      if (!match) continue;
      const timestamp = parseTime(finiteNumber(match[3]));
      if (timestamp !== null && blockTime !== null && timestamp !== blockTime && Object.keys(metrics).length) flush();
      if (timestamp !== null) blockTime = timestamp;
      metrics[metricNameWithoutLabels(match[1])] = Number(match[2]);
    }
    flush();
    return points;
  }

  function lanePoint(fields, source, time) {
    const elapsed = finiteNumber(fields.t);
    if (elapsed === null) return null;
    const flow = String(fields.flow || "unknown");
    const lane = String(fields.lane ?? fields.lane_id ?? "unknown");
    const metrics = {};
    for (const [key, alias] of Object.entries(laneAliases)) {
      let raw = fields[key];
      if (typeof raw === "string") raw = raw.replace(/ms$/, "");
      const number = finiteNumber(raw);
      if (number !== null) metrics[alias] = number;
    }
    return {
      source, kind: "lane", time: time ?? null, elapsed, group: `${flow} / lane ${lane}`,
      metrics, meta: { flow, lane },
    };
  }

  function parseLaneLine(line, source) {
    if (!/(?:^|\s)lane\s+t=/.test(line)) return null;
    return lanePoint(parseLogfmt(line.replace(/^(?:.*?\s)?lane\s+/, "")), source, null);
  }

  function objectMetrics(record) {
    const metrics = {};
    for (const [key, value] of Object.entries(record || {})) {
      const number = finiteNumber(value);
      if (number !== null) metrics[key] = number;
    }
    return metrics;
  }

  function queqiaoMetrics(record) {
    const metrics = {};
    for (const [key, value] of Object.entries(record || {})) {
      if (!key.startsWith("queqiao_")) continue;
      if (value === true || value === "true") metrics[key] = 1;
      else if (value === false || value === "false") metrics[key] = 0;
      else {
        const number = finiteNumber(value);
        if (number !== null) metrics[key] = number;
      }
    }
    return metrics;
  }

  function parseEventObject(record, source, data) {
    if (!record || typeof record !== "object" || Array.isArray(record)) return false;
    if (record.msg === "lane performance snapshot" || record.type === "lane_metrics") {
      const lane = lanePoint(record, source, parseTime(valueFrom(record, ["time", "timestamp", "ts"])));
      if (lane) data.points.push(lane);
      return !!lane;
    }
    const flow = parseFlowRecord(record, source);
    if (flow) {
      data.flows.push(flow);
      data.events.push({ source, time: flow.time, type: "flow", status: flow.status, message: flow.message, details: flow });
      return true;
    }
    const type = String(record.type || "");
    let addedMetrics = false;
    const flatMetrics = queqiaoMetrics(record);
    if (Object.keys(flatMetrics).length) {
      data.points.push({
        source, kind: "metrics",
        time: parseTime(valueFrom(record, ["time", "timestamp", "ts", "started_utc"])),
        elapsed: finiteNumber(record.elapsed_seconds), group: String(record.role || record.label || source),
        metrics: normalizePrometheus(flatMetrics), meta: { type: type || "metrics", telemetry_schema: record.telemetry_schema },
      });
      addedMetrics = true;
    }
    if (record.metrics && typeof record.metrics === "object") {
      const time = parseTime(valueFrom(record, ["started_utc", "time", "timestamp", "ts"]));
      data.points.push({
        source, kind: "metrics", time, elapsed: finiteNumber(record.elapsed_seconds), group: String(record.label || source),
        metrics: normalizePrometheus(objectMetrics(record.metrics)), meta: { type: type || "metrics", index: record.index },
      });
      addedMetrics = true;
    }
    // Successful periodic snapshots are chart samples, not operator events.
    // Keeping one event per scrape would bury failures and flow transitions in
    // a long capture. Failed scrapes have no metrics and continue below.
    if (addedMetrics && (type === "metrics" || type === "resource" || record.msg === "performance snapshot") && record.status !== "failed" && !record.error) return true;
    const level = String(record.level || "").toLowerCase();
    const message = String(valueFrom(record, ["msg", "message"]) || "");
    const lifecycle = /(?:runtime logging initialized|runtime configuration|runtime stopped|listener ready)$/.test(message);
    if (type || record.status || record.error || level === "warn" || level === "warning" || level === "error" || lifecycle) {
      const seconds = finiteNumber(record.seconds);
      const event = {
        source,
        time: parseTime(valueFrom(record, ["started_utc", "time", "timestamp", "ts"])),
        type: type || "log",
        status: String(record.status || (record.error || level === "error" ? "failed" : (level === "warn" || level === "warning" ? "warning" : "ok"))),
        message: String(record.error ? `${message}: ${record.error}` : (record.note || message)),
        details: record,
      };
      data.events.push(event);
      if (seconds !== null) {
        data.points.push({
          source, kind: "probe", time: event.time, elapsed: null,
          group: `${source} / ${event.type}`,
          metrics: { probe_latency_ms: seconds * 1000, probe_success: event.status === "ok" ? 1 : 0 },
          meta: { type: event.type, status: event.status },
        });
      }
      return true;
    }
    return !!record.metrics;
  }

  function isBenchmarkReport(object) {
    return !!(object && typeof object === "object" && object.path && Array.isArray(object.trials) && Array.isArray(object.summary));
  }

  function addBenchmark(report, source, data) {
    data.benchmarks.push({ source, report });
    for (const trial of report.trials || []) {
      data.tabular.push(Object.assign({ source, table: "benchmark" }, trial));
    }
    for (const latency of report.latency || []) {
      data.tabular.push(Object.assign({ source, table: "latency" }, latency));
    }
  }

  function splitDelimited(line, delimiter) {
    // Harness TSV/CSV output is intentionally simple; retain empty cells.
    return line.split(delimiter).map((cell) => cell.trim());
  }

  function parseDelimited(text, source, data) {
    const lines = text.split(/\r?\n/).filter((line) => line.trim() && !line.trim().startsWith("#"));
    if (lines.length < 2) return false;
    const delimiter = lines[0].includes("\t") ? "\t" : (lines[0].includes(",") ? "," : null);
    if (!delimiter) return false;
    const headers = splitDelimited(lines[0], delimiter);
    if (headers.length < 2 || headers.some((header) => !header)) return false;
    let rows = 0;
    for (const line of lines.slice(1)) {
      const cells = splitDelimited(line, delimiter);
      if (cells.length !== headers.length) continue;
      const record = { source, table: "delimited" };
      headers.forEach((header, index) => {
        const number = finiteNumber(cells[index]);
        record[header] = number === null ? cells[index] : number;
      });
      data.tabular.push(record);
      const latencySeconds = finiteNumber(valueFrom(record, ["total_seconds", "seconds", "latency_seconds"]));
      const rateMbits = finiteNumber(valueFrom(record, ["mbits_per_sec", "mbps"]));
      const speed = finiteNumber(valueFrom(record, ["speed_bytes_per_sec", "bytes_per_second"]));
      const loss = finiteNumber(valueFrom(record, ["loss_pct", "loss_percent", "packet_loss_percent"]));
      const metrics = {};
      if (latencySeconds !== null) metrics.transfer_seconds = latencySeconds;
      if (rateMbits !== null) metrics.benchmark_throughput_bps = rateMbits * 1e6;
      if (speed !== null) metrics.benchmark_throughput_bps = speed * 8;
      if (loss !== null) metrics.configured_loss_percent = loss;
      if (Object.keys(metrics).length) {
        data.points.push({
          source, kind: "table", time: parseTime(valueFrom(record, ["time", "timestamp", "started_utc"])),
          elapsed: rows, group: String(valueFrom(record, ["label", "stack", "flows"]) || source), metrics, meta: record,
        });
      }
      rows++;
    }
    return rows > 0;
  }

  function parseWholeJSON(object, source, data) {
    if (isBenchmarkReport(object)) {
      addBenchmark(object, source, data);
      addSource(data, source, "benchmark JSON", (object.trials || []).length || 1);
      return true;
    }
    if (Array.isArray(object)) {
      let handled = false;
      for (const item of object) {
        if (isBenchmarkReport(item)) {
          addBenchmark(item, source, data);
          handled = true;
        } else {
          handled = parseEventObject(item, source, data) || handled;
        }
      }
      if (handled) addSource(data, source, "JSON records", object.length || 1);
      return handled;
    }
    if (object && typeof object === "object" && object.metrics_delta && object.udp_attempts !== undefined) {
      data.events.push({
        source, time: parseTime(object.finished_utc), type: "soak summary",
        status: object.passed ? "ok" : "failed",
        message: `${object.udp_successes || 0}/${object.udp_attempts || 0} UDP and ${object.https_successes || 0}/${object.https_attempts || 0} HTTPS probes succeeded`,
        details: object,
      });
      addSource(data, source, "field-soak summary", 1);
      return true;
    }
    if (object && typeof object === "object" && (object.rss_kib !== undefined || object.file_descriptors !== undefined)) {
      const metrics = {};
      if (finiteNumber(object.rss_kib) !== null) metrics.process_rss_bytes = Number(object.rss_kib) * 1024;
      if (finiteNumber(object.file_descriptors) !== null) metrics.process_file_descriptors = Number(object.file_descriptors);
      data.points.push({ source, kind: "process", time: null, elapsed: null, group: source, metrics });
      addSource(data, source, "process snapshot", 1);
      return true;
    }
    if (object && typeof object === "object" && object.label && object.duration_seconds !== undefined && object.interval_seconds !== undefined) {
      data.formats["field-soak manifest"] = (data.formats["field-soak manifest"] || 0) + 1;
      return true;
    }
    if (parseEventObject(object, source, data)) {
      addSource(data, source, "JSON record", 1);
      return true;
    }
    const metricKeys = Object.keys(object || {}).filter((key) => key.startsWith("queqiao_"));
    if (metricKeys.length) {
      const metrics = {};
      for (const key of metricKeys) {
        const number = finiteNumber(object[key]);
        if (number !== null) metrics[key] = number;
      }
      data.points.push({ source, kind: "metrics", time: null, elapsed: null, group: source, metrics: normalizePrometheus(metrics) });
      addSource(data, source, "metrics JSON", metricKeys.length);
      return true;
    }
    return false;
  }

  function mergeDataset(target, incoming) {
    for (const key of ["points", "benchmarks", "flows", "events", "tabular", "warnings"]) target[key].push(...incoming[key]);
    for (const source of incoming.sources) if (!target.sources.includes(source)) target.sources.push(source);
    for (const [format, count] of Object.entries(incoming.formats)) target.formats[format] = (target.formats[format] || 0) + count;
    return target;
  }

  function parseText(source, text) {
    const data = emptyDataset();
    const trimmed = String(text || "").trim();
    if (!trimmed) {
      data.warnings.push(`${source}: empty file`);
      return data;
    }

    try {
      const object = JSON.parse(trimmed);
      if (parseWholeJSON(object, source, data)) return finalize(data);
    } catch (_) { /* Fall through to line-oriented formats. */ }

    let jsonLines = 0;
    let laneLines = 0;
    let flowLines = 0;
    let telemetryLines = 0;
    let invalidJSONLines = 0;
    for (const raw of trimmed.split(/\r?\n/)) {
      const line = raw.trim();
      if (!line) continue;
      const lane = parseLaneLine(line, source);
      if (lane) {
        data.points.push(lane);
        laneLines++;
        continue;
      }
      if (line[0] === "{") {
        try {
          const record = JSON.parse(line);
          if (parseEventObject(record, source, data)) jsonLines++;
        } catch (_) { invalidJSONLines++; }
        continue;
      }
      if (/\bmsg="performance snapshot"/.test(line)) {
        const record = parseLogfmt(line);
        const metrics = queqiaoMetrics(record);
        if (Object.keys(metrics).length) {
          data.points.push({
            source, kind: "metrics", time: parseTime(record.time), elapsed: finiteNumber(record.elapsed_seconds),
            group: String(record.role || source), metrics: normalizePrometheus(metrics),
            meta: { type: "metrics", telemetry_schema: record.telemetry_schema },
          });
          telemetryLines++;
          continue;
        }
      }
      if (/\bmsg="lane performance snapshot"/.test(line)) {
        const record = parseLogfmt(line);
        const lane = lanePoint(record, source, parseTime(record.time));
        if (lane) {
          data.points.push(lane);
          laneLines++;
          continue;
        }
      }
      if (/\bmsg=(?:"[^\"]*flow (?:complete|ended with error)[^\"]*"|\S+)/i.test(line)) {
        const record = parseLogfmt(line);
        const flow = parseFlowRecord(record, source);
        if (flow) {
          data.flows.push(flow);
          data.events.push({ source, time: flow.time, type: "flow", status: flow.status, message: flow.message, details: flow });
          flowLines++;
          continue;
        }
      }
      if (/\blevel=(?:INFO|WARN|WARNING|ERROR)\b/i.test(line)) {
        const record = parseLogfmt(line);
        if (parseEventObject(record, source, data)) jsonLines++;
      }
    }
    if (laneLines) addSource(data, source, "lane trace", laneLines);
    if (jsonLines) addSource(data, source, "JSON Lines", jsonLines);
    if (flowLines) addSource(data, source, "structured log", flowLines);
    if (telemetryLines) addSource(data, source, "runtime telemetry", telemetryLines);

    const prometheus = parsePrometheus(trimmed, source, null);
    if (prometheus.length) {
      data.points.push(...prometheus);
      addSource(data, source, "Prometheus", prometheus.length);
    }

    if (!laneLines && !jsonLines && !flowLines && !prometheus.length) {
      if (parseDelimited(trimmed, source, data)) addSource(data, source, "tabular", data.tabular.length);
    }
    if (!data.sources.length) data.warnings.push(`${source}: no supported performance records found${invalidJSONLines ? ` (${invalidJSONLines} malformed JSON line(s))` : ""}`);
    return finalize(data);
  }

  function pointX(point, index) {
    if (point.time !== null && Number.isFinite(point.time)) return point.time / 1000;
    if (point.elapsed !== null && Number.isFinite(point.elapsed)) return point.elapsed;
    return index;
  }

  function deriveRates(data) {
    const groups = new Map();
    data.points.forEach((point, index) => {
      point._order = index;
      point.x = pointX(point, index);
      const key = `${point.source}\u0000${point.group}\u0000${point.kind}`;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(point);
    });
    const counters = [
      ["quic_bytes_received_total", "throughput_down_bps", 8],
      ["quic_bytes_sent_total", "throughput_up_bps", 8],
      ["application_bytes_down_total", "application_throughput_down_bps", 8],
      ["application_bytes_up_total", "application_throughput_up_bps", 8],
      ["quic_bytes_lost_total", "loss_bytes_per_second", 1],
      ["quic_packets_sent_total", "packets_sent_per_second", 1],
      ["quic_packets_received_total", "packets_received_per_second", 1],
      ["quic_packets_lost_total", "loss_packets_per_second", 1],
      ["controller_bytes_lost_total", "controller_loss_bytes_per_second", 1],
      ["controller_packets_lost_total", "controller_loss_packets_per_second", 1],
      ["bytes_reissued_total", "reissued_bytes_per_second", 1],
    ];
    for (const points of groups.values()) {
      points.sort((a, b) => a.x - b.x || a._order - b._order);
      for (let index = 1; index < points.length; index++) {
        const previous = points[index - 1];
        const current = points[index];
        const seconds = current.x - previous.x;
        if (!(seconds > 0)) continue;
        for (const [counter, rate, factor] of counters) {
          const before = previous.metrics[counter];
          const after = current.metrics[counter];
          if (!Number.isFinite(before) || !Number.isFinite(after) || after < before) continue;
          current.metrics[rate] = (after - before) * factor / seconds;
        }
        const sent = current.metrics.quic_bytes_sent_total - previous.metrics.quic_bytes_sent_total;
        const lost = current.metrics.quic_bytes_lost_total - previous.metrics.quic_bytes_lost_total;
        if (Number.isFinite(sent) && Number.isFinite(lost) && sent > 0 && sent >= 0 && lost >= 0) {
          current.metrics.byte_loss_percent = 100 * lost / sent;
        }
        const packetsSent = current.metrics.quic_packets_sent_total - previous.metrics.quic_packets_sent_total;
        const packetsLost = current.metrics.quic_packets_lost_total - previous.metrics.quic_packets_lost_total;
        if (Number.isFinite(packetsSent) && Number.isFinite(packetsLost) && packetsSent > 0 && packetsLost >= 0) {
          current.metrics.packet_loss_percent = 100 * packetsLost / packetsSent;
        }
      }
    }
  }

  function finalize(data) {
    deriveRates(data);
    data.points.sort((a, b) => a.x - b.x || a._order - b._order);
    return data;
  }

  return {
    emptyDataset, mergeDataset, parseText, parseLogfmt, parseDurationMillis,
    parseCodedSubstrate, parsePrometheus, parseLaneLine, normalizePrometheus,
    finalize,
  };
});
