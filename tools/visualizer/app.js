(function () {
  "use strict";

  const Parser = window.QueqiaoLogParser;
  const colors = ["#62e6b3", "#73d7e6", "#f2bd64", "#b69af3", "#ff7b76", "#72a2f2", "#e985bd", "#b4d66f"];
  const state = { data: Parser.emptyDataset(), source: "*", group: "*", imports: [] };

  const elements = {};
  for (const id of [
    "drop-zone", "file-input", "folder-input", "paste-button", "paste-panel", "paste-input",
    "parse-paste", "cancel-paste", "workspace", "dataset-title", "import-summary",
    "source-filter", "group-filter", "clear-button", "warnings", "kpi-grid", "timeline-empty",
    "chart-grid", "diagnostics", "fec-section", "fec-content", "benchmark-section", "benchmark-content",
    "records-section", "records-table", "import-manifest", "toast",
  ]) elements[id] = document.getElementById(id);

  function escapeHTML(value) {
    return String(value ?? "").replace(/[&<>"']/g, (character) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    })[character]);
  }

  function median(values) {
    const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
    if (!sorted.length) return null;
    const middle = Math.floor(sorted.length / 2);
    return sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2;
  }

  function percentile(values, fraction) {
    const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);
    if (!sorted.length) return null;
    return sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(sorted.length * fraction) - 1))];
  }

  function sum(values) { return values.filter(Number.isFinite).reduce((total, value) => total + value, 0); }
  function max(values) { const usable = values.filter(Number.isFinite); return usable.length ? Math.max(...usable) : null; }
  function latest(values) { const usable = values.filter(Number.isFinite); return usable.length ? usable[usable.length - 1] : null; }
  function clamp(value, lower, upper) { return Math.max(lower, Math.min(upper, value)); }

  function formatSI(value, unit, digits) {
    if (!Number.isFinite(value)) return "—";
    const absolute = Math.abs(value);
    const scales = [[1e12, "T"], [1e9, "G"], [1e6, "M"], [1e3, "k"]];
    const found = scales.find(([scale]) => absolute >= scale);
    if (found) return `${(value / found[0]).toFixed(digits ?? (absolute / found[0] >= 100 ? 0 : 1))} ${found[1]}${unit}`;
    return `${value.toFixed(digits ?? (absolute >= 100 ? 0 : 1))} ${unit}`;
  }

  function formatBits(value) { return formatSI(value, "bit/s"); }
  function formatBytes(value) { return formatSI(value, "B"); }
  function formatRateBytes(value) { return formatSI(value, "B/s"); }
  function formatMillis(value) {
    if (!Number.isFinite(value)) return "—";
    if (Math.abs(value) >= 1000) return `${(value / 1000).toFixed(value >= 10000 ? 1 : 2)} s`;
    return `${value.toFixed(value >= 100 ? 1 : 2)} ms`;
  }
  function formatPercent(value) { return Number.isFinite(value) ? `${value.toFixed(value >= 10 ? 1 : 2)}%` : "—"; }
  function formatCount(value) { return Number.isFinite(value) ? new Intl.NumberFormat().format(Math.round(value)) : "—"; }
  function formatTime(value) {
    if (!Number.isFinite(value)) return "relative time";
    return new Date(value).toLocaleString([], { hour12: false });
  }

  function showToast(message) {
    elements.toast.textContent = message;
    elements.toast.classList.add("show");
    clearTimeout(showToast.timer);
    showToast.timer = setTimeout(() => elements.toast.classList.remove("show"), 2800);
  }

  function visiblePoints() {
    return state.data.points.filter((point) =>
      (state.source === "*" || point.source === state.source) &&
      (state.group === "*" || point.group === state.group));
  }

  function sourceVisible(record) { return state.source === "*" || record.source === state.source; }

  function metricValues(points, names, transform) {
    const result = [];
    for (const point of points) {
      for (const name of names) {
        const value = point.metrics[name];
        if (Number.isFinite(value)) {
          result.push(transform ? transform(value, point) : value);
          break;
        }
      }
    }
    return result;
  }

  function filteredFlows() { return state.data.flows.filter(sourceVisible); }
  function filteredBenchmarks() { return state.data.benchmarks.filter(sourceVisible); }
  function filteredTabular() { return state.data.tabular.filter(sourceVisible); }
  function filteredEvents() { return state.data.events.filter(sourceVisible); }

  function latestFECFlow(flows) {
    const candidates = flows.filter((flow) => flow.fec);
    if (!candidates.length) return null;
    return candidates.reduce((latestFlow, flow) => {
      if (Number.isFinite(flow.time) && (!Number.isFinite(latestFlow.time) || flow.time >= latestFlow.time)) return flow;
      return latestFlow;
    }, candidates[candidates.length - 1]);
  }

  function updateFilters() {
    const oldSource = state.source;
    elements["source-filter"].innerHTML = '<option value="*">All sources</option>' + state.data.sources.map((source) =>
      `<option value="${escapeHTML(source)}">${escapeHTML(source)}</option>`).join("");
    if (oldSource !== "*" && state.data.sources.includes(oldSource)) elements["source-filter"].value = oldSource;
    else state.source = "*";
    updateGroupFilter();
  }

  function updateGroupFilter() {
    const groups = [...new Set(state.data.points.filter((point) => state.source === "*" || point.source === state.source).map((point) => point.group))].sort();
    const oldGroup = state.group;
    elements["group-filter"].innerHTML = '<option value="*">All groups</option>' + groups.map((group) =>
      `<option value="${escapeHTML(group)}">${escapeHTML(group)}</option>`).join("");
    if (oldGroup !== "*" && groups.includes(oldGroup)) elements["group-filter"].value = oldGroup;
    else state.group = "*";
  }

  function renderKPI() {
    const points = visiblePoints();
    const flows = filteredFlows();
    const reports = filteredBenchmarks();
    const tables = filteredTabular();
    const throughputs = metricValues(points, ["throughput_down_bps", "application_throughput_down_bps", "benchmark_throughput_bps", "throughput_up_bps", "application_throughput_up_bps"]);
    throughputs.push(...flows.map((flow) => flow.throughput_bps));
    throughputs.push(...tables.map((row) => Number.isFinite(row.mbits_per_sec) ? row.mbits_per_sec * 1e6 : (Number.isFinite(row.speed_bytes_per_sec) ? row.speed_bytes_per_sec * 8 : null)));
    const rtts = metricValues(points, ["smoothed_rtt_ms", "latest_rtt_ms", "probe_latency_ms"]);
    for (const benchmark of reports) if (Number.isFinite(benchmark.report.path.rtt_ms)) rtts.push(benchmark.report.path.rtt_ms);
    const losses = metricValues(points, ["packet_loss_percent", "byte_loss_percent", "configured_loss_percent"]);
    for (const benchmark of reports) if (Number.isFinite(benchmark.report.path.loss_percent)) losses.push(benchmark.report.path.loss_percent);
    const fecFlow = latestFECFlow(flows);
    const fec = fecFlow && fecFlow.fec;
    const recovered = fec ? fec.recovered : 0;
    const residual = fec ? fec.lost : 0;
    const sent = fec ? fec.sent : 0;
    const repairs = fec ? fec.repairs : 0;
    const completions = [];
    for (const flow of flows) completions.push(flow.status === "complete" ? 100 : 0);
    for (const benchmark of reports) for (const summary of benchmark.report.summary || []) completions.push(summary.completion_rate * 100);
    for (const row of tables) {
      if (row.complete !== undefined) completions.push((row.complete === true || row.complete === 1 || row.complete === "1") ? 100 : 0);
    }
    for (const event of filteredEvents().filter((event) => ["udp", "https", "flow"].includes(event.type))) completions.push(event.status === "ok" || event.status === "complete" ? 100 : 0);

    const cards = [
      { label: "Peak observed goodput", value: formatBits(max(throughputs)), note: throughputs.length ? `${throughputs.length} rate sample${throughputs.length === 1 ? "" : "s"}` : "requires byte deltas or benchmark rates", tone: "good" },
      { label: "Smoothed / probe RTT p95", value: formatMillis(percentile(rtts, .95)), note: rtts.length ? `median ${formatMillis(median(rtts))}` : "no RTT samples", tone: percentile(rtts, .95) > 800 ? "warn" : "" },
      { label: "Peak packet / path loss", value: formatPercent(max(losses)), note: losses.length ? `median ${formatPercent(median(losses))}` : "needs packet counters or path metadata", tone: max(losses) > 10 ? "bad" : max(losses) > 2 ? "warn" : "" },
      { label: "Latest FEC effectiveness", value: formatPercent(recovered + residual > 0 ? 100 * recovered / (recovered + residual) : null), note: `${formatCount(recovered)} recovered · ${formatCount(residual)} residual`, tone: residual > recovered * .1 ? "warn" : recovered ? "good" : "" },
      { label: "Latest FEC repair share", value: formatPercent(sent > 0 ? 100 * repairs / sent : null), note: `${formatCount(repairs)} repair / ${formatCount(sent)} sent`, tone: "" },
      { label: "Completion rate", value: formatPercent(median(completions)), note: completions.length ? `${completions.length} recorded outcome${completions.length === 1 ? "" : "s"}` : "no completion evidence", tone: median(completions) < 95 ? "bad" : completions.length ? "good" : "" },
    ];
    elements["kpi-grid"].innerHTML = cards.map((card) => `
      <article class="kpi ${card.tone}">
        <div class="kpi-label">${escapeHTML(card.label)}</div>
        <div class="kpi-value">${escapeHTML(card.value)}</div>
        <div class="kpi-note">${escapeHTML(card.note)}</div>
      </article>`).join("");
  }

  const chartDefinitions = [
    {
      title: "Goodput and controller rates", unit: "bit/s", format: formatBits,
      series: [
        ["throughput_down_bps", "Goodput ↓", (value) => value],
        ["throughput_up_bps", "Goodput ↑", (value) => value],
        ["application_throughput_down_bps", "Completed bytes ↓", (value) => value],
        ["application_throughput_up_bps", "Completed bytes ↑", (value) => value],
        ["benchmark_throughput_bps", "Benchmark", (value) => value],
        ["delivery_sample_bytes_per_second", "Delivery sample", (value) => value * 8],
        ["ack_rate_bytes_per_second", "ACK rate", (value) => value * 8],
        ["send_rate_bytes_per_second", "Send rate", (value) => value * 8],
        ["pacing_bytes_per_second", "Pacing", (value) => value * 8],
        ["max_bandwidth_bytes_per_second", "Max bandwidth", (value) => value * 8],
      ],
    },
    {
      title: "Round-trip and request latency", unit: "ms", format: formatMillis,
      series: [
        ["smoothed_rtt_ms", "Smoothed RTT"], ["latest_rtt_ms", "Latest RTT"],
        ["min_rtt_ms", "Controller min RTT"], ["probe_latency_ms", "Probe latency"],
        ["mean_residency_ms", "Chunk residency"], ["max_residency_ms", "Max residency"],
      ],
    },
    {
      title: "Loss and path erasure", unit: "% / packets·s⁻¹", format: (value) => value.toFixed(value >= 10 ? 1 : 2),
      series: [
        ["packet_loss_percent", "Packet loss %"],
        ["byte_loss_percent", "Byte loss %"],
        ["configured_loss_percent", "Configured loss %"],
        ["erasure_floor_ratio", "Erasure floor %", (value) => value * 100],
        ["loss_packets_per_second", "Packet loss / s"],
        ["controller_loss_packets_per_second", "Controller loss / s"],
      ],
    },
    {
      title: "Congestion and scheduler windows", unit: "bytes", format: formatBytes,
      series: [
        ["cwnd_bytes", "Congestion window"], ["inflight_bytes", "Bytes in flight"],
        ["scheduler_window_bytes", "Lane window"], ["lane_held_bytes", "Lane held"],
        ["lane_queued_bytes", "Lane queued"], ["flow_held_bytes", "Flow held"],
      ],
    },
    {
      title: "Recovery and retransmission work", unit: "events / rate", format: (value) => formatSI(value, ""),
      series: [
        ["reissued_bytes_per_second", "Reissued B/s"], ["chunks_reissued_total", "Chunks reissued"],
        ["lane_failures_total", "Lane failures"], ["controller_in_recovery", "In recovery"],
        ["transient_udp_send_errors_total", "Transient UDP sends"],
        ["acks_scheduled", "ACKs scheduled"], ["ack_write_ms", "ACK write ms"],
      ],
    },
    {
      title: "Controller state", unit: "state / samples", format: (value) => formatSI(value, ""),
      series: [
        ["controller_mode", "Mode"], ["controller_round", "Round"],
        ["app_limited_samples_total", "App-limited samples"],
        ["non_app_limited_samples_total", "Non-app samples"],
        ["quic_lanes", "QUIC lanes"], ["active_flows", "Active flows"],
      ],
    },
  ];

  function displayX(points) {
    const timeBases = new Map();
    for (const point of points) {
      if (Number.isFinite(point.time)) {
        const current = timeBases.get(point.source);
        if (!Number.isFinite(current) || point.time < current) timeBases.set(point.source, point.time);
      }
    }
    return (point, fallback) => Number.isFinite(point.time)
      ? (point.time - timeBases.get(point.source)) / 1000
      : (Number.isFinite(point.elapsed) ? point.elapsed : fallback);
  }

  function buildChartSeries(points, definition) {
    const getX = displayX(points);
    const groups = [...new Set(points.map((point) => point.group))];
    const result = [];
    let colorIndex = 0;
    for (const [metric, label, transform] of definition.series) {
      for (const group of groups) {
        const samples = [];
        points.forEach((point, index) => {
          if (point.group !== group) return;
          const value = point.metrics[metric];
          if (!Number.isFinite(value)) return;
          samples.push({ x: getX(point, index), y: transform ? transform(value, point) : value, point });
        });
        if (!samples.length) continue;
        result.push({
          label: groups.length > 1 ? `${label} · ${group}` : label,
          shortLabel: label,
          color: colors[colorIndex++ % colors.length], samples,
        });
        if (result.length >= 18) return result;
      }
    }
    return result;
  }

  function niceTicks(minimum, maximum, count) {
    if (minimum === maximum) return [minimum];
    const rough = (maximum - minimum) / Math.max(1, count - 1);
    const power = Math.pow(10, Math.floor(Math.log10(Math.abs(rough))));
    const normalized = rough / power;
    const step = (normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10) * power;
    const start = Math.floor(minimum / step) * step;
    const ticks = [];
    for (let value = start; value <= maximum + step * .5; value += step) if (value >= minimum - step * .01) ticks.push(value);
    return ticks.slice(0, count + 2);
  }

  function renderChart(definition, series, chartIndex) {
    const width = 720, height = 250, left = 62, right = 15, top = 15, bottom = 28;
    const all = series.flatMap((item) => item.samples);
    let xMin = Math.min(...all.map((sample) => sample.x));
    let xMax = Math.max(...all.map((sample) => sample.x));
    let yMin = Math.min(0, ...all.map((sample) => sample.y));
    let yMax = Math.max(...all.map((sample) => sample.y));
    if (xMin === xMax) { xMin -= .5; xMax += .5; }
    if (yMin === yMax) { yMin -= .5; yMax += .5; }
    yMax += (yMax - yMin) * .08;
    const plotWidth = width - left - right, plotHeight = height - top - bottom;
    const sx = (value) => left + (value - xMin) / (xMax - xMin) * plotWidth;
    const sy = (value) => top + (yMax - value) / (yMax - yMin) * plotHeight;
    const yTicks = niceTicks(yMin, yMax, 4);
    const xTicks = niceTicks(xMin, xMax, 5);
    const pathFor = (samples) => {
      const usable = samples.length > 700 ? samples.filter((_, index) => index % Math.ceil(samples.length / 700) === 0 || index === samples.length - 1) : samples;
      return usable.map((sample, index) => `${index ? "L" : "M"}${sx(sample.x).toFixed(2)},${sy(sample.y).toFixed(2)}`).join(" ");
    };
    const svg = `
      <svg viewBox="0 0 ${width} ${height}" preserveAspectRatio="none" aria-label="${escapeHTML(definition.title)} chart">
        ${yTicks.map((tick) => `<line x1="${left}" y1="${sy(tick)}" x2="${width - right}" y2="${sy(tick)}" stroke="rgba(145,169,161,.13)"/><text x="${left - 9}" y="${sy(tick) + 3}" text-anchor="end" fill="#789089" font-size="9" font-family="monospace">${escapeHTML(definition.format(tick))}</text>`).join("")}
        ${xTicks.map((tick) => `<line x1="${sx(tick)}" y1="${top}" x2="${sx(tick)}" y2="${height - bottom}" stroke="rgba(145,169,161,.06)"/><text x="${sx(tick)}" y="${height - 8}" text-anchor="middle" fill="#789089" font-size="9" font-family="monospace">${escapeHTML(tick.toFixed(xMax - xMin < 10 ? 1 : 0))}s</text>`).join("")}
        ${series.map((item) => `<path d="${pathFor(item.samples)}" fill="none" stroke="${item.color}" stroke-width="1.7" vector-effect="non-scaling-stroke" opacity=".9"/>`).join("")}
        <line id="cursor-${chartIndex}" x1="${left}" y1="${top}" x2="${left}" y2="${height - bottom}" stroke="#d7eee6" stroke-width="1" opacity="0" vector-effect="non-scaling-stroke"/>
        <rect data-chart-overlay="${chartIndex}" x="${left}" y="${top}" width="${plotWidth}" height="${plotHeight}" fill="transparent"/>
      </svg>`;
    return { svg, bounds: { width, height, left, right, top, bottom, xMin, xMax, sx }, all };
  }

  function attachChartHover(card, chart, definition, series, index) {
    const overlay = card.querySelector(`[data-chart-overlay="${index}"]`);
    const cursor = card.querySelector(`#cursor-${index}`);
    const tooltip = card.querySelector(".chart-tooltip");
    const wrap = card.querySelector(".chart-wrap");
    overlay.addEventListener("pointermove", (event) => {
      const rect = overlay.getBoundingClientRect();
      const ratio = clamp((event.clientX - rect.left) / rect.width, 0, 1);
      const targetX = chart.bounds.xMin + ratio * (chart.bounds.xMax - chart.bounds.xMin);
      let nearest = null;
      for (const sample of chart.all) if (!nearest || Math.abs(sample.x - targetX) < Math.abs(nearest.x - targetX)) nearest = sample;
      if (!nearest) return;
      const tolerance = Math.max((chart.bounds.xMax - chart.bounds.xMin) * .015, .001);
      const values = [];
      for (const item of series) {
        let candidate = null;
        for (const sample of item.samples) if (!candidate || Math.abs(sample.x - nearest.x) < Math.abs(candidate.x - nearest.x)) candidate = sample;
        if (candidate && Math.abs(candidate.x - nearest.x) <= tolerance * 6) values.push({ item, sample: candidate });
      }
      const svgX = chart.bounds.sx(nearest.x);
      cursor.setAttribute("x1", svgX); cursor.setAttribute("x2", svgX); cursor.setAttribute("opacity", "0.65");
      tooltip.hidden = false;
      tooltip.style.left = `${clamp(event.clientX - wrap.getBoundingClientRect().left, 4, wrap.clientWidth - 180)}px`;
      tooltip.style.top = `${clamp(event.clientY - wrap.getBoundingClientRect().top, 30, wrap.clientHeight - 30)}px`;
      const sampleTime = values.find((value) => Number.isFinite(value.sample.point.time));
      tooltip.innerHTML = `<strong>t + ${nearest.x.toFixed(2)} s</strong>${sampleTime ? `<span>${escapeHTML(formatTime(sampleTime.sample.point.time))}</span>` : ""}` + values.slice(0, 8).map(({ item, sample }) =>
        `<div><i style="color:${item.color}">●</i> ${escapeHTML(item.shortLabel)} <b>${escapeHTML(definition.format(sample.y))}</b></div>`).join("");
    });
    overlay.addEventListener("pointerleave", () => { cursor.setAttribute("opacity", "0"); tooltip.hidden = true; });
  }

  function renderCharts() {
    const points = visiblePoints();
    elements["chart-grid"].innerHTML = "";
    let rendered = 0;
    for (const definition of chartDefinitions) {
      const series = buildChartSeries(points, definition);
      if (!series.length) continue;
      const chart = renderChart(definition, series, rendered);
      const card = document.createElement("article");
      card.className = "chart-card panel";
      card.innerHTML = `<div class="chart-head"><h3>${escapeHTML(definition.title)}</h3><span>elapsed time</span></div><div class="chart-wrap">${chart.svg}<div class="chart-tooltip" hidden></div></div><div class="legend">${series.map((item) => `<span class="legend-item"><i class="legend-swatch" style="background:${item.color}"></i>${escapeHTML(item.label)}</span>`).join("")}</div>`;
      elements["chart-grid"].appendChild(card);
      attachChartHover(card, chart, definition, series, rendered);
      rendered++;
    }
    elements["timeline-empty"].hidden = rendered > 0;
  }

  function diagnosticCard(title, status, signal, message) {
    return `<article class="diagnostic"><div class="diagnostic-top"><h3>${escapeHTML(title)}</h3><span class="signal ${status}">${escapeHTML(signal)}</span></div><p>${escapeHTML(message)}</p></article>`;
  }

  function renderDiagnostics() {
    const points = visiblePoints();
    const cards = [];
    const inflation = [];
    const flightPressure = [];
    for (const point of points) {
      const smooth = point.metrics.smoothed_rtt_ms;
      const minimum = point.metrics.min_rtt_ms;
      if (smooth > 0 && minimum > 0) inflation.push(smooth / minimum);
      const cwnd = point.metrics.cwnd_bytes;
      const inflight = point.metrics.inflight_bytes;
      if (cwnd > 0 && inflight >= 0) flightPressure.push(inflight / cwnd);
    }
    const rttP95 = percentile(inflation, .95);
    cards.push(diagnosticCard("RTT inflation", rttP95 > 2 ? "bad" : rttP95 > 1.35 ? "warn" : Number.isFinite(rttP95) ? "good" : "neutral",
      Number.isFinite(rttP95) ? `${rttP95.toFixed(2)}× p95` : "no signal",
      Number.isFinite(rttP95) ? `Smoothed RTT is ${rttP95.toFixed(2)}× the controller minimum at p95. Sustained inflation usually points to queueing.` : "Needs simultaneous smoothed and minimum RTT samples."));
    const pressureP95 = percentile(flightPressure, .95);
    cards.push(diagnosticCard("Flight pressure", pressureP95 > 1.02 ? "bad" : pressureP95 > .9 ? "warn" : Number.isFinite(pressureP95) ? "good" : "neutral",
      Number.isFinite(pressureP95) ? formatPercent(pressureP95 * 100) : "no signal",
      Number.isFinite(pressureP95) ? `Bytes in flight consume ${formatPercent(pressureP95 * 100)} of the congestion window at p95.` : "Needs congestion-window and bytes-in-flight samples."));
    const lossRate = max(metricValues(points, ["loss_packets_per_second", "controller_loss_packets_per_second"]));
    cards.push(diagnosticCard("Loss activity", lossRate > 100 ? "bad" : lossRate > 5 ? "warn" : Number.isFinite(lossRate) ? "good" : "neutral",
      Number.isFinite(lossRate) ? `${formatSI(lossRate, " pkt/s")}` : "no rate",
      Number.isFinite(lossRate) ? `Peak counter delta was ${formatSI(lossRate, " lost packets/s")}. Compare its timing with RTT, recovery, and rate collapse.` : "Packet-loss counters need at least two timestamped snapshots."));
    const recoveries = metricValues(points, ["controller_in_recovery"]);
    const recoveryShare = recoveries.length ? 100 * recoveries.filter((value) => value > 0).length / recoveries.length : null;
    cards.push(diagnosticCard("Recovery occupancy", recoveryShare > 35 ? "bad" : recoveryShare > 10 ? "warn" : Number.isFinite(recoveryShare) ? "good" : "neutral",
      formatPercent(recoveryShare),
      Number.isFinite(recoveryShare) ? `The sender reports recovery in ${formatPercent(recoveryShare)} of sampled observations.` : "No controller recovery samples were present."));
    const fecFlow = latestFECFlow(filteredFlows());
    const recovered = fecFlow ? fecFlow.fec.recovered : 0;
    const lost = fecFlow ? fecFlow.fec.lost : 0;
    const effectiveness = recovered + lost > 0 ? 100 * recovered / (recovered + lost) : null;
    cards.push(diagnosticCard("FEC residual", lost > recovered * .1 ? "bad" : lost ? "warn" : recovered ? "good" : "neutral",
      Number.isFinite(effectiveness) ? `${formatPercent(effectiveness)} repaired` : "no FEC",
      Number.isFinite(effectiveness) ? `The latest FEC observation recovered ${formatCount(recovered)} erased symbols; ${formatCount(lost)} escaped the coding window.` : "Load flow-completion logs to inspect FEC outcomes."));
    const residency = percentile(metricValues(points, ["max_residency_ms"]), .95);
    const rtt = median(metricValues(points, ["smoothed_rtt_ms"]));
    const residencyRatio = residency > 0 && rtt > 0 ? residency / rtt : null;
    cards.push(diagnosticCard("Scheduler residency", residencyRatio > 3 ? "bad" : residencyRatio > 1.5 ? "warn" : Number.isFinite(residencyRatio) ? "good" : "neutral",
      Number.isFinite(residencyRatio) ? `${residencyRatio.toFixed(2)}× RTT` : "no signal",
      Number.isFinite(residencyRatio) ? `Max-residency p95 is ${formatMillis(residency)}, against a ${formatMillis(rtt)} median RTT.` : "Lane traces supply scheduler residency and RTT together."));
    const failures = max(metricValues(points, ["lane_failures_total"])) || 0;
    const reissueRate = max(metricValues(points, ["reissued_bytes_per_second"]));
    cards.push(diagnosticCard("Lane rescue", failures > 0 ? "warn" : Number.isFinite(reissueRate) ? "good" : "neutral",
      failures ? `${formatCount(failures)} failures` : Number.isFinite(reissueRate) ? "stable" : "no signal",
      failures || Number.isFinite(reissueRate) ? `Observed ${formatCount(failures)} cumulative lane failures and a ${formatRateBytes(reissueRate || 0)} peak reissue rate.` : "Lane trace or metrics events are required."));
    const probes = filteredEvents().filter((event) => event.type === "udp" || event.type === "https");
    const failed = probes.filter((event) => event.status !== "ok").length;
    cards.push(diagnosticCard("Probe reliability", failed ? "bad" : probes.length ? "good" : "neutral",
      probes.length ? `${probes.length - failed}/${probes.length}` : "no probes",
      probes.length ? `${probes.length - failed} probes succeeded and ${failed} failed in the selected field-soak evidence.` : "Load field-soak events.jsonl or UDP probe TSV."));
    elements.diagnostics.innerHTML = cards.join("");
  }

  function renderFEC() {
    const flows = filteredFlows().filter((flow) => flow.fec);
    elements["fec-section"].hidden = flows.length === 0;
    if (!flows.length) return;
    const latestFlow = latestFECFlow(flows);
    const recovered = latestFlow.fec.recovered;
    const lost = latestFlow.fec.lost;
    const sent = latestFlow.fec.sent;
    const repairs = latestFlow.fec.repairs;
    const effectiveness = recovered + lost > 0 ? 100 * recovered / (recovered + lost) : 0;
    const rows = flows.slice(-200).reverse();
    elements["fec-content"].innerHTML = `<div class="fec-layout">
      <article class="fec-gauge panel">
        <div class="ring" style="--value:${clamp(effectiveness, 0, 100)}"><div><div class="ring-value">${escapeHTML(formatPercent(effectiveness))}</div><div class="ring-label">erasure recovery</div></div></div>
        <div class="fec-stats">
          <div class="fec-stat"><span>Repair share</span><strong>${escapeHTML(formatPercent(sent ? 100 * repairs / sent : null))}</strong></div>
          <div class="fec-stat"><span>Residual</span><strong>${escapeHTML(formatCount(lost))}</strong></div>
          <div class="fec-stat"><span>Recovered</span><strong>${escapeHTML(formatCount(recovered))}</strong></div>
          <div class="fec-stat"><span>Flow coded payload</span><strong>${escapeHTML(formatBytes(latestFlow.data_coded))}</strong></div>
        </div>
      </article>
      <article class="panel table-panel"><div class="table-scroll"><table><thead><tr><th>Time</th><th>Endpoint / flow</th><th>Coding</th><th>K / N</th><th>Rate</th><th>Window</th><th>Observed loss</th><th>Erasure floor</th><th>Burst factor</th><th>Predicted residual</th><th>Sent</th><th>Repairs</th><th>Recovered</th><th>Residual</th><th>Payload split</th></tr></thead><tbody>
        ${rows.map((flow) => `<tr><td>${escapeHTML(Number.isFinite(flow.time) ? formatTime(flow.time) : "—")}</td><td title="${escapeHTML(flow.session_id || "")}">${escapeHTML([flow.endpoint, flow.transport, flow.flow_id && `#${flow.flow_id}`].filter(Boolean).join(" · "))}</td><td class="${flow.fec.coding ? "status-ok" : ""}">${flow.fec.coding ? "active" : "off"}</td><td title="${escapeHTML(flow.fec.reason || "")}">${escapeHTML(Number.isFinite(flow.fec.plan_k) && Number.isFinite(flow.fec.plan_n) ? `${flow.fec.plan_k} / ${flow.fec.plan_n}` : "—")}</td><td>${escapeHTML(Number.isFinite(flow.fec.rate) ? flow.fec.rate.toFixed(2) : "—")}</td><td>${escapeHTML(formatCount(flow.fec.window))}</td><td>${escapeHTML(formatPercent(Number.isFinite(flow.fec.observed_loss) ? flow.fec.observed_loss * 100 : null))}</td><td>${escapeHTML(formatPercent(Number.isFinite(flow.fec.erasure_floor) ? flow.fec.erasure_floor * 100 : null))}</td><td>${escapeHTML(Number.isFinite(flow.fec.burst_factor) ? `${flow.fec.burst_factor.toFixed(2)}×` : "—")}</td><td>${escapeHTML(formatPercent(Number.isFinite(flow.fec.estimated_residual) ? flow.fec.estimated_residual * 100 : null))}</td><td>${escapeHTML(formatCount(flow.fec.sent))}</td><td>${escapeHTML(formatCount(flow.fec.repairs))}</td><td class="status-ok">${escapeHTML(formatCount(flow.fec.recovered))}</td><td class="${flow.fec.lost ? "status-failed" : ""}">${escapeHTML(formatCount(flow.fec.lost))}</td><td>${escapeHTML(`${formatCount(flow.data_coded)} coded / ${formatCount(flow.data_stream)} stream`)}</td></tr>`).join("")}
      </tbody></table></div></article>
    </div>`;
  }

  function tabularSummaries(rows) {
    const groups = new Map();
    for (const row of rows) {
      let rate = finite(row.mbits_per_sec);
      if (rate === null && finite(row.speed_bytes_per_sec) !== null) rate = row.speed_bytes_per_sec * 8 / 1e6;
      if (rate === null) continue;
      const label = String(row.label || row.stack || (row.flows !== undefined ? `${row.flows} flows` : row.source));
      if (!groups.has(label)) groups.set(label, []);
      groups.get(label).push({ rate, complete: row.complete });
    }
    return [...groups].map(([label, values]) => ({
      label, median: median(values.map((value) => value.rate)), trials: values.length,
      completion: values.some((value) => value.complete !== undefined) ? 100 * values.filter((value) => value.complete === true || value.complete === 1 || value.complete === "1").length / values.length : null,
    }));
  }

  function finite(value) { const number = Number(value); return Number.isFinite(number) ? number : null; }

  function renderBenchmarks() {
    const reports = filteredBenchmarks();
    const summaries = [];
    for (const benchmark of reports) {
      for (const item of benchmark.report.summary || []) summaries.push({
        source: benchmark.source, label: `${item.stack} · ${item.flows} flow${item.flows === 1 ? "" : "s"}`,
        stack: item.stack, flows: item.flows, trials: item.trials, completed: item.completed,
        completion: item.completion_rate * 100, median: item.median_mbits_all_trials,
        mean: item.mean_mbits_all_trials, worst: item.worst_mbits_all_trials,
        p95: item.interactive_median && item.interactive_median.p95_ms,
      });
    }
    for (const item of tabularSummaries(filteredTabular().filter((row) => row.table === "delimited"))) summaries.push({
      source: "tabular", label: item.label, stack: item.label, flows: "—", trials: item.trials,
      completed: "—", completion: item.completion, median: item.median, mean: null, worst: null, p95: null,
    });
    elements["benchmark-section"].hidden = summaries.length === 0;
    if (!summaries.length) return;
    const maximum = Math.max(...summaries.map((item) => item.median || 0), 1);
    elements["benchmark-content"].innerHTML = `<div class="benchmark-layout">
      <article class="bar-panel panel"><h3>Median goodput, all trials</h3>${summaries.map((item) => `<div class="bar-row"><div class="bar-label" title="${escapeHTML(item.label)}">${escapeHTML(item.label)}</div><div class="bar-track"><div class="bar-fill" style="width:${clamp(100 * item.median / maximum, 0, 100)}%"></div></div><div class="bar-value">${escapeHTML(Number.isFinite(item.median) ? `${item.median.toFixed(2)} Mbit/s` : "—")}</div></div>`).join("")}</article>
      <article class="panel table-panel"><div class="table-scroll"><table><thead><tr><th>Stack / cell</th><th>Trials</th><th>Completed</th><th>Completion</th><th>Median</th><th>Mean</th><th>Worst</th><th>Interactive p95</th></tr></thead><tbody>${summaries.map((item) => `<tr><td>${escapeHTML(item.label)}</td><td>${escapeHTML(item.trials)}</td><td>${escapeHTML(item.completed)}</td><td class="${Number.isFinite(item.completion) && item.completion < 95 ? "status-failed" : "status-ok"}">${escapeHTML(formatPercent(item.completion))}</td><td>${escapeHTML(Number.isFinite(item.median) ? `${item.median.toFixed(2)} Mbit/s` : "—")}</td><td>${escapeHTML(Number.isFinite(item.mean) ? `${item.mean.toFixed(2)} Mbit/s` : "—")}</td><td>${escapeHTML(Number.isFinite(item.worst) ? `${item.worst.toFixed(2)} Mbit/s` : "—")}</td><td>${escapeHTML(formatMillis(item.p95))}</td></tr>`).join("")}</tbody></table></div></article>
    </div>`;
  }

  function renderRecords() {
    const records = [];
    for (const event of filteredEvents()) records.push({
      time: event.time, source: event.source, type: event.type, status: event.status,
      detail: event.type === "log" ? JSON.stringify(event.details || {}) : (event.message || JSON.stringify(event.details || {})),
    });
    for (const row of filteredTabular()) records.push({
      time: parseTimeFromRow(row), source: row.source, type: row.table,
      status: row.complete === undefined ? "record" : (row.complete === true || row.complete === 1 || row.complete === "1" ? "ok" : "failed"),
      detail: Object.entries(row).filter(([key]) => !["source", "table"].includes(key)).map(([key, value]) => `${key}=${value}`).join(" "),
    });
    records.sort((a, b) => (Number.isFinite(b.time) ? b.time : -Infinity) - (Number.isFinite(a.time) ? a.time : -Infinity));
    elements["records-section"].hidden = records.length === 0;
    if (!records.length) return;
    elements["records-table"].innerHTML = `<thead><tr><th>Time</th><th>Source</th><th>Type</th><th>Status</th><th>Details</th></tr></thead><tbody>${records.slice(0, 200).map((record) => `<tr><td>${escapeHTML(Number.isFinite(record.time) ? formatTime(record.time) : "—")}</td><td>${escapeHTML(record.source)}</td><td>${escapeHTML(record.type)}</td><td class="status-${escapeHTML(record.status)}">${escapeHTML(record.status)}</td><td class="details-cell" title="${escapeHTML(record.detail)}">${escapeHTML(record.detail)}</td></tr>`).join("")}</tbody>`;
  }

  function parseTimeFromRow(row) {
    for (const key of ["time", "timestamp", "started_utc"]) {
      if (row[key] !== undefined) {
        const time = Date.parse(row[key]);
        if (Number.isFinite(time)) return time;
      }
    }
    return null;
  }

  function renderManifest() {
    const formats = Object.entries(state.data.formats);
    elements["import-manifest"].innerHTML = state.imports.map((item) => `<span class="manifest-chip"><strong>${escapeHTML(item.name)}</strong> · ${escapeHTML(formatBytes(item.size))}</span>`).join("") +
      formats.map(([format, count]) => `<span class="manifest-chip"><strong>${escapeHTML(format)}</strong> · ${formatCount(count)} records</span>`).join("");
  }

  function renderWarnings() {
    elements.warnings.innerHTML = state.data.warnings.map((warning) => `<div class="warning">${escapeHTML(warning)}</div>`).join("");
  }

  function render() {
    const records = state.data.points.length + state.data.flows.length + state.data.events.length + state.data.tabular.length;
    elements.workspace.hidden = records === 0 && state.data.benchmarks.length === 0;
    if (elements.workspace.hidden) return;
    elements["dataset-title"].textContent = state.source === "*" ? "All performance evidence" : state.source;
    elements["import-summary"].textContent = `${state.data.sources.length} source${state.data.sources.length === 1 ? "" : "s"} · ${formatCount(records)} records`;
    renderWarnings();
    renderKPI();
    renderCharts();
    renderDiagnostics();
    renderFEC();
    renderBenchmarks();
    renderRecords();
    renderManifest();
  }

  async function loadFiles(files) {
    const ignoredNames = new Set(["SHA256SUMS", "source.patch", "source-status.txt"]);
    const accepted = [...files].filter((file) => !file.name.startsWith(".") && !ignoredNames.has(file.name) && file.size > 0);
    if (!accepted.length) { showToast("No readable files selected."); return; }
    let loaded = 0;
    for (const file of accepted) {
      if (file.size > 128 * 1024 * 1024) {
        state.data.warnings.push(`${file.name}: skipped because it exceeds the 128 MiB browser safety limit`);
        continue;
      }
      try {
        const parsed = Parser.parseText(file.webkitRelativePath || file.name, await file.text());
        Parser.mergeDataset(state.data, parsed);
        state.imports.push({ name: file.webkitRelativePath || file.name, size: file.size });
        loaded++;
      } catch (error) {
        state.data.warnings.push(`${file.name}: ${error.message || error}`);
      }
    }
    Parser.finalize(state.data);
    updateFilters();
    render();
    showToast(`Loaded ${loaded} file${loaded === 1 ? "" : "s"} locally.`);
  }

  function loadPaste() {
    const text = elements["paste-input"].value;
    if (!text.trim()) { showToast("Paste some log content first."); return; }
    const name = `pasted-${state.imports.length + 1}`;
    Parser.mergeDataset(state.data, Parser.parseText(name, text));
    state.imports.push({ name, size: new Blob([text]).size });
    Parser.finalize(state.data);
    elements["paste-input"].value = "";
    closePaste();
    updateFilters(); render(); showToast("Parsed pasted content locally.");
  }

  function openPaste() {
    elements["paste-panel"].hidden = false;
    elements["drop-zone"].querySelector(".drop-copy").hidden = true;
    elements["drop-zone"].querySelector(".drop-actions").hidden = true;
    elements["paste-input"].focus();
  }

  function closePaste() {
    elements["paste-panel"].hidden = true;
    elements["drop-zone"].querySelector(".drop-copy").hidden = false;
    elements["drop-zone"].querySelector(".drop-actions").hidden = false;
  }

  function clearAll() {
    state.data = Parser.emptyDataset(); state.imports = []; state.source = "*"; state.group = "*";
    elements.workspace.hidden = true;
    updateFilters();
    showToast("Dashboard cleared.");
  }

  elements["file-input"].addEventListener("change", (event) => loadFiles(event.target.files));
  elements["folder-input"].addEventListener("change", (event) => loadFiles(event.target.files));
  elements["paste-button"].addEventListener("click", openPaste);
  elements["cancel-paste"].addEventListener("click", closePaste);
  elements["parse-paste"].addEventListener("click", loadPaste);
  elements["clear-button"].addEventListener("click", clearAll);
  elements["source-filter"].addEventListener("change", (event) => { state.source = event.target.value; state.group = "*"; updateGroupFilter(); render(); });
  elements["group-filter"].addEventListener("change", (event) => { state.group = event.target.value; render(); });

  for (const eventName of ["dragenter", "dragover"]) elements["drop-zone"].addEventListener(eventName, (event) => { event.preventDefault(); elements["drop-zone"].classList.add("dragging"); });
  for (const eventName of ["dragleave", "drop"]) elements["drop-zone"].addEventListener(eventName, (event) => { event.preventDefault(); elements["drop-zone"].classList.remove("dragging"); });
  elements["drop-zone"].addEventListener("drop", (event) => loadFiles(event.dataTransfer.files));
})();
