(function () {
  "use strict";

  var toast = document.getElementById("toast");
  var lastDataTime = null;

  function showToast(msg, type, duration) {
    duration = duration || 3000;
    toast.textContent = msg;
    toast.className = "toast " + (type || "info");
    toast.hidden = false;
    clearTimeout(toast._timer);
    toast._timer = setTimeout(function () { toast.hidden = true; }, duration);
  }

  function formatTime(ts) {
    if (!ts) return "--";
    var d = new Date(ts);
    return d.toLocaleTimeString("zh-CN", { hour12: false });
  }

  function setStatus(connected) {
    var el = document.getElementById("connection-status");
    el.innerHTML = connected ? "● 已连接" : "○ 连接断开";
    el.style.background = connected
      ? "rgba(63, 185, 80, 0.15)"
      : "rgba(248, 81, 73, 0.15)";
    el.style.color = connected ? "#3fb950" : "#f85149";
  }

  // ─── 摘要轮询 ────────────────────────────────────────
  function fetchSummary() {
    fetch("/ui/api/summary")
      .then(function (r) { return r.json(); })
      .then(function (data) {
        var m = data.metrics || {};
        var qpsEl = document.getElementById("qps");
        var p99El = document.getElementById("p99");
        var errEl = document.getElementById("error-rate");
        var actEl = document.getElementById("active-conns");

        if (qpsEl) qpsEl.textContent = m.qps !== null ? m.qps.toFixed(1) : "--";
        if (p99El) p99El.textContent = m.p99_latency_ms !== null ? Math.round(m.p99_latency_ms) : "--";
        if (errEl) errEl.textContent = m.error_rate !== null ? m.error_rate.toFixed(2) : "--";
        if (actEl) actEl.textContent = m.active_connections || 0;

        // Provider 列表
        var listEl = document.getElementById("provider-list");
        var countEl = document.getElementById("provider-count");
        if (listEl) {
          var providers = data.providers || [];
          countEl.textContent = providers.length + " 个";
          if (providers.length === 0) {
            listEl.innerHTML =
              '<div class="provider-item empty"><span class="dot dot-idle"></span>' +
              '<span class="provider-name">等待数据...</span><span class="provider-status">idle</span></div>';
          } else {
            listEl.innerHTML = providers.map(function (p) {
              return '<div class="provider-item">' +
                '<span class="dot dot-' + p.status + '"></span>' +
                '<span class="provider-name">' + (p.name || p.id) + '</span>' +
                '<span class="provider-meta">请求: ' + (p.request_count || 0) +
                ' · 错误: ' + (p.error_count || 0) +
                ' · 平均: ' + (p.avg_latency_ms ? Math.round(p.avg_latency_ms) + "ms" : "--") +
                '</span>' +
                '<span class="provider-status ' + p.status + '">' + p.status + '</span>' +
                '</div>';
            }).join("");
          }
        }

        // 时间戳
        var tsEl = document.getElementById("last-updated");
        if (tsEl) tsEl.textContent = "更新于 " + formatTime(Date.now());
      })
      .catch(function () { /* 静默失败，轮询会继续 */ });
  }

  // ─── 图表初始化 ──────────────────────────────────────
  var chartData = [[], []]; // [timestamp, count]
  var uplot = null;

  function initChart() {
    try {
      uplot = new uPlot({
        title: "请求量趋势",
        series: [
          { name: "时间" },
          { name: "请求", label: "请求数", stroke: "#58a6ff", width: 2, fill: "rgba(88, 166, 255, 0.15)", points: { show: false } }
        ],
        axes: [
          {
            values: function (self, vals) { return vals.map(function (v) {
              if (v < 0) return "";
              return new Date(v * 1000).toLocaleTimeString("zh-CN", { hour12: false });
            }); }
          },
          { label: "请求数", grid: { stroke: "#30363d" } }
        ],
        width: 900,
        height: 180,
        padding: [8, 20, 10, 20],
      }, chartData, document.getElementById("chart"));
    } catch (e) {
      // uPlot CDN 可能未加载，忽略
    }
  }

  function updateChart(ts, count) {
    if (!uplot || !ts) return;
    var now = Math.floor(Date.now() / 1000);
    // 只保留最近 60 秒的数据
    var cutoff = now - 60;

    if (chartData[0].length > 0 && chartData[0][chartData[0].length - 1] === ts) {
      // 同一时间戳更新
      chartData[1][chartData[1].length - 1] = count;
    } else {
      chartData[0].push(ts);
      chartData[1].push(count);
    }

    // 清理过期数据
    while (chartData[0].length > 0 && chartData[0][0] < cutoff) {
      chartData[0].shift();
      chartData[1].shift();
    }

    uplot.setData(chartData);
  }

  // ─── 日志表格 ────────────────────────────────────────
  var logMaxRows = 100;

  function appendLogRow(log) {
    var tbody = document.getElementById("log-tbody");
    if (!tbody) return;

    // 移除空行提示
    var empty = tbody.querySelector(".empty-row");
    if (empty) empty.remove();

    var tr = document.createElement("tr");

    var statusClass = "status-ok";
    var statusText = log.status;
    if (log.status >= 400) {
      statusClass = "status-err";
    } else if (log.status >= 300) {
      statusClass = "status-warn";
    }

    tr.innerHTML =
      '<td>' + formatTime(log.time) + '</td>' +
      '<td>' + (log.method || "-") + '</td>' +
      '<td>' + (log.path || "/v1/chat/completions") + '</td>' +
      '<td>' + (log.model || "-") + '</td>' +
      '<td>' + (log.provider || "-") + '</td>' +
      '<td class="' + statusClass + '">' + statusText + '</td>' +
      '<td>' + (log.latency != null ? log.latency + "ms" : "--") + '</td>';

    tbody.insertBefore(tr, tbody.firstChild);

    // 限制行数
    while (tbody.children.length > logMaxRows) {
      tbody.removeChild(tbody.lastChild);
    }

    // 更新计数
    var countEl = document.getElementById("log-count");
    if (countEl) countEl.textContent = tbody.children.length + " 条记录";
  }

  // ─── SSE 连接 ────────────────────────────────────────
  var metricsSSE = null;
  var logsSSE = null;

  function connectSSE() {
    setStatus(true);

    // Metrics SSE
    try {
      metricsSSE = new EventSource("/ui/api/metrics");
      metricsSSE.addEventListener("metrics", function (e) {
        try {
          var data = JSON.parse(e.data);
          updateChart(data.timestamp, data.count);
        } catch (err) { /* 忽略解析错误 */ }
      });
      metricsSSE.addEventListener("error", function () {
        console.warn("Metrics SSE error, reconnecting...");
      });
    } catch (e) {
      console.error("Failed to create metrics SSE:", e);
    }

    // Logs SSE
    try {
      logsSSE = new EventSource("/ui/api/logs");
      logsSSE.addEventListener("log", function (e) {
        try {
          var data = JSON.parse(e.data);
          appendLogRow(data);
        } catch (err) { /* 忽略解析错误 */ }
      });
      logsSSE.addEventListener("open", function () {
        showToast("SSE 日志流已连接", "info", 2000);
      });
    } catch (e) {
      console.error("Failed to create logs SSE:", e);
    }
  }

  function disconnectSSE() {
    if (metricsSSE) { metricsSSE.close(); metricsSSE = null; }
    if (logsSSE) { logsSSE.close(); logsSSE = null; }
    setStatus(false);
  }

  // ─── 启动 ────────────────────────────────────────────
  initChart();
  fetchSummary();

  // 每 2 秒轮询摘要
  setInterval(fetchSummary, 2000);

  // 尝试连接 SSE
  setTimeout(connectSSE, 500);

  // SSE 断线后每 5 秒重试
  window.addEventListener("error", function () {
    setTimeout(connectSSE, 5000);
  });
})();
