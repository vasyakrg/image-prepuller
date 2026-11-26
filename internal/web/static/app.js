(() => {
  const state = {
    user: null,
    clusters: [],
    activeCluster: "",
    snapshot: null,
    activeTab: "workloads",
    expanded: new Set(),
    pollTimer: null,
    autoRefresh: true,
  };

  const $ = (id) => document.getElementById(id);

  function fmtBytes(bytes) {
    if (!bytes) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    let v = bytes;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(i >= 3 ? 1 : 0)} ${units[i]}`;
  }

  function fmtTime(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    return d.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function esc(str) {
    return String(str ?? "")
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;");
  }

  let toastTimer = null;
  function showToast(msg) {
    const el = $("toast");
    el.textContent = msg;
    el.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.add("hidden"), 3000);
  }

  async function api(path, opts = {}) {
    const res = await fetch(path, {
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
      ...opts,
    });
    if (res.status === 401 && path !== "/api/v1/auth/login") {
      showLogin();
      throw new Error("unauthorized");
    }
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
    return data;
  }

  function showLogin() {
    stopPolling();
    state.user = null;
    $("app-view").classList.add("hidden");
    $("login-view").classList.remove("hidden");
    $("login-user").focus();
  }

  function showApp(username) {
    state.user = username;
    $("user-badge").textContent = username;
    $("login-view").classList.add("hidden");
    $("app-view").classList.remove("hidden");
    refresh();
    startPolling();
  }

  function startPolling() {
    stopPolling();
    if (!state.autoRefresh) return;
    state.pollTimer = setInterval(() => {
      if (state.user && state.autoRefresh) refresh();
    }, 5000);
  }

  function stopPolling() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
  }

  async function refresh() {
    try {
      const cl = await api("/api/v1/clusters");
      state.clusters = cl.clusters || [];
      if (!state.activeCluster || !state.clusters.some((c) => c.id === state.activeCluster)) {
        state.activeCluster = cl.defaultCluster || (state.clusters[0] && state.clusters[0].id) || "";
      }
      renderClusterSelect();
      const qs = state.activeCluster ? `?cluster=${encodeURIComponent(state.activeCluster)}` : "";
      state.snapshot = await api(`/api/v1/snapshot${qs}`);
      render();
    } catch (err) {
      if (err.message !== "unauthorized") showToast(`Ошибка обновления: ${err.message}`);
    }
  }

  function renderClusterSelect() {
    const sel = $("cluster-select");
    sel.innerHTML = state.clusters.map((c) => {
      const tag = c.role === "local" ? "hub" : "remote";
      const dot = c.online ? "●" : "○";
      return `<option value="${esc(c.id)}">${dot} ${esc(c.name)} (${tag} · ${c.readyWorkers}/${c.totalWorkers})</option>`;
    }).join("");
    sel.value = state.activeCluster;
  }

  function stateLabel(st) {
    switch (st) {
      case "present": return { text: "Present", cls: "pill-green" };
      case "pulling": return { text: "Pulling...", cls: "pill-blue" };
      case "failed": return { text: "Failed", cls: "pill-red" };
      case "paused_disk": return { text: "Paused: Disk", cls: "pill-purple" };
      case "offline": return { text: "Offline", cls: "pill-neutral" };
      default: return { text: "Missing", cls: "pill-amber" };
    }
  }

  function render() {
    const snap = state.snapshot;
    if (!snap) return;

    // cluster selector rendered in renderClusterSelect()
    $("mock-badge").classList.toggle("hidden", !snap.mockMode);
    $("tab-nodes-count").textContent = String(snap.nodes.length);

    const s = snap.summary;
    $("kpi-workloads").textContent = String(s.totalWorkloads);
    $("kpi-workloads-sub").textContent = `${s.deployments} Deploy · ${s.statefulSets} STS · ${s.daemonSets} DS · ${s.uniqueImages} images`;
    $("kpi-coverage").textContent = `${s.coveragePercent.toFixed(1)}%`;
    $("kpi-coverage-sub").textContent = `${s.presentSlots} / ${s.totalSlots} image-node slots`;
    $("kpi-workers").textContent = `${s.readyWorkers} / ${s.totalWorkers}`;
    const offline = s.totalWorkers - s.readyWorkers;
    $("kpi-workers-sub").textContent = `${offline > 0 ? offline + " offline" : "все онлайн"}${s.diskPausedNodes > 0 ? " · " + s.diskPausedNodes + " disk paused" : ""}`;
    $("kpi-warnings").textContent = String(s.policyWarnings);
    $("kpi-warnings").style.color = s.policyWarnings > 0 ? "var(--amber)" : "var(--green)";

    // Populate namespace select
    const nsSel = $("filter-ns");
    const prevNs = nsSel.value;
    const namespaces = [...new Set(snap.workloads.map((w) => w.namespace))].sort();
    nsSel.innerHTML = '<option value="">Namespace: Все</option>' +
      namespaces.map((ns) => `<option value="${esc(ns)}">${esc(ns)}</option>`).join("");
    nsSel.value = namespaces.includes(prevNs) ? prevNs : "";

    renderWorkloads(snap);
    renderNodes(snap);
    renderActivity(snap);
  }

  function renderWorkloads(snap) {
    const q = $("filter-search").value.trim().toLowerCase();
    const nsFilter = $("filter-ns").value;
    const statusFilter = $("filter-status").value;

    const filtered = snap.workloads.filter((w) => {
      if (nsFilter && w.namespace !== nsFilter) return false;
      if (q) {
        const hay = `${w.namespace} ${w.kind} ${w.name} ${w.images.map((i) => i.image).join(" ")}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      const hasWarn = w.images.some((i) => (i.policyViolations || []).length > 0);
      const hasIssue = w.images.some((i) => i.nodes.some((n) => n.state !== "present"));
      if (statusFilter === "warnings" && !hasWarn) return false;
      if (statusFilter === "issues" && !hasIssue) return false;
      return true;
    });

    const tbody = $("workloads-tbody");
    if (filtered.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="muted" style="text-align:center;padding:28px">Нет сервисов по выбранным фильтрам</td></tr>';
      return;
    }

    tbody.innerHTML = filtered.map((w) => {
      const key = `${w.namespace}/${w.kind}/${w.name}`;
      const isExp = state.expanded.has(key);
      const allViolations = w.images.flatMap((i) => i.policyViolations || []);
      const policyHtml = allViolations.length === 0
        ? '<span class="pill pill-green">✓ OK</span>'
        : `<span class="pill pill-amber" title="${esc(allViolations.map((v) => v.message).join("; "))}">⚠ ${allViolations.length} Warn</span>`;

      // Aggregate node dots across images of this workload
      const nodeDots = snap.nodes.map((n) => {
        const states = w.images.map((img) => {
          const found = img.nodes.find((x) => x.node === n.name);
          return found ? found.state : "missing";
        });
        let agg = "present";
        if (states.includes("pulling")) agg = "pulling";
        else if (states.includes("failed")) agg = "failed";
        else if (states.includes("offline")) agg = "offline";
        else if (states.includes("paused_disk")) agg = "paused_disk";
        else if (states.includes("missing")) agg = "missing";
        const shortName = n.name.replace(/^talos-/, "");
        return `<span class="node-dot" title="${esc(n.name)}: ${agg}"><span class="dot dot-${agg}"></span>${esc(shortName)}</span>`;
      }).join("");

      const imgList = w.images.map((i) =>
        `<div class="mono">${i.isInit ? '<span class="pill pill-neutral" style="margin-right:4px">init</span>' : ""}${esc(i.image)}</div>`
      ).join("");

      let html = `
        <tr class="workload-row" data-key="${esc(key)}">
          <td class="muted">${isExp ? "▾" : "▸"}</td>
          <td>
            <div><strong>${esc(w.name)}</strong></div>
            <div class="muted" style="font-size:12px">${esc(w.namespace)} · ${esc(w.kind)} · replicas: ${w.replicas}</div>
          </td>
          <td>${imgList}</td>
          <td>${policyHtml}</td>
          <td><div class="node-dots">${nodeDots}</div></td>
          <td class="right">
            <button class="btn btn-secondary btn-xs btn-pull-wl"
              data-ns="${esc(w.namespace)}" data-kind="${esc(w.kind)}" data-name="${esc(w.name)}">
              ↻ Pull All
            </button>
          </td>
        </tr>
      `;

      if (isExp) {
        const nodeHeaders = snap.nodes.map((n) => `<th class="mono">${esc(n.name)}</th>`).join("");
        const imgRows = w.images.map((img) => {
          const cells = img.nodes.map((ns) => {
            const lbl = stateLabel(ns.state);
            const canPull = ns.state !== "offline" && ns.state !== "pulling";
            return `
              <td>
                <div style="display:flex;justify-content:space-between;align-items:center;gap:6px">
                  <span class="pill ${lbl.cls}">${lbl.text}</span>
                  <button class="btn btn-secondary btn-xs btn-pull-cell"
                    data-image="${esc(img.image)}" data-node="${esc(ns.node)}"
                    ${canPull ? "" : "disabled"}>↻ Pull</button>
                </div>
                <div class="cell-meta">${ns.sizeBytes ? fmtBytes(ns.sizeBytes) + " · " : ""}chk ${fmtTime(ns.lastChecked)}</div>
                ${ns.error ? `<div class="cell-err">${esc(ns.error)}</div>` : ""}
              </td>
            `;
          }).join("");

          return `
            <tr>
              <td>
                <div class="mono">${esc(img.image)}</div>
                <div class="cell-meta">container: ${esc(img.containerName)} · policy: ${esc(img.pullPolicy || "default")}</div>
              </td>
              ${cells}
            </tr>
          `;
        }).join("");

        html += `
          <tr>
            <td colspan="6" class="expand-cell">
              <table class="matrix-table">
                <thead><tr><th>Image</th>${nodeHeaders}</tr></thead>
                <tbody>${imgRows}</tbody>
              </table>
            </td>
          </tr>
        `;
      }
      return html;
    }).join("");

    // Bind row expand
    tbody.querySelectorAll(".workload-row").forEach((tr) => {
      tr.addEventListener("click", (e) => {
        if (e.target.closest("button")) return;
        const k = tr.dataset.key;
        if (state.expanded.has(k)) state.expanded.delete(k);
        else state.expanded.add(k);
        renderWorkloads(state.snapshot);
      });
    });

    // Bind Pull All
    tbody.querySelectorAll(".btn-pull-wl").forEach((btn) => {
      btn.addEventListener("click", async (e) => {
        e.stopPropagation();
        try {
          const res = await api("/api/v1/workloads/pull", {
            method: "POST",
            body: JSON.stringify({ cluster: state.activeCluster, namespace: btn.dataset.ns, kind: btn.dataset.kind, name: btn.dataset.name }),
          });
          showToast(`Запущен pull на ${res.triggered} слотах`);
          await refresh();
        } catch (err) {
          showToast(`Ошибка: ${err.message}`);
        }
      });
    });

    // Bind single cell Pull
    tbody.querySelectorAll(".btn-pull-cell").forEach((btn) => {
      btn.addEventListener("click", async (e) => {
        e.stopPropagation();
        try {
          await api("/api/v1/pull", {
            method: "POST",
            body: JSON.stringify({ cluster: state.activeCluster, image: btn.dataset.image, node: btn.dataset.node }),
          });
          showToast(`Pull запущен: ${btn.dataset.node}`);
          await refresh();
        } catch (err) {
          showToast(`Ошибка: ${err.message}`);
        }
      });
    });
  }

  function renderNodes(snap) {
    const grid = $("nodes-grid");
    grid.innerHTML = snap.nodes.map((n) => {
      const pct = n.imageFsCapacityBytes > 0 ? (n.imageFsUsedBytes / n.imageFsCapacityBytes) * 100 : 0;
      let barColor = "var(--green)";
      if (pct >= n.kubeletGcThreshold * 100) barColor = "var(--red)";
      else if (pct >= n.diskPauseThreshold * 100) barColor = "var(--purple)";
      else if (pct >= 70) barColor = "var(--amber)";

      const badges = [
        n.ready ? '<span class="pill pill-green">Ready</span>' : '<span class="pill pill-red">NotReady</span>',
        n.agentUp ? '<span class="pill pill-green">Agent UP</span>' : '<span class="pill pill-red">Agent DOWN</span>',
        n.diskPaused ? '<span class="pill pill-purple">Disk Paused</span>' : "",
      ].filter(Boolean).join(" ");

      const cached = (n.cachedImages || []).map((ci) =>
        `<div class="cached-item"><span class="mono" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(ci.image)}</span><span class="muted">${fmtBytes(ci.sizeBytes)}</span></div>`
      ).join("") || '<div class="muted" style="font-size:12px">Нет данных об образах</div>';

      return `
        <div class="node-card">
          <div class="node-card-header">
            <strong class="mono" style="font-size:14px">${esc(n.name)}</strong>
            <div style="display:flex;gap:6px;flex-wrap:wrap">${badges}</div>
          </div>
          <div style="display:flex;justify-content:space-between;font-size:12px">
            <span class="muted">EPHEMERAL / ImageFS</span>
            <span class="mono">${fmtBytes(n.imageFsUsedBytes)} / ${fmtBytes(n.imageFsCapacityBytes)} (${pct.toFixed(1)}%)</span>
          </div>
          <div class="disk-bar-wrap" title="Белые метки: 78% пауза авто-пула, 85% Kubelet GC">
            <div class="disk-bar-fill" style="width:${Math.min(pct, 100).toFixed(1)}%;background:${barColor}"></div>
            <div class="disk-marker" style="left:${(n.diskPauseThreshold * 100).toFixed(0)}%"></div>
            <div class="disk-marker" style="left:${(n.kubeletGcThreshold * 100).toFixed(0)}%"></div>
          </div>
          <div style="display:flex;justify-content:space-between;font-size:11.5px" class="muted">
            <span>Образов: ${n.cachedImageCount} · Active pulls: ${n.activePulls}</span>
            <span>Seen: ${fmtTime(n.lastSeen)}</span>
          </div>
          <div class="cached-list">${cached}</div>
        </div>
      `;
    }).join("");
  }

  function renderActivity(snap) {
    const tbody = $("activity-tbody");
    if (!snap.activity || snap.activity.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="muted" style="text-align:center;padding:24px">Пока нет событий pull</td></tr>';
      return;
    }
    tbody.innerHTML = snap.activity.map((ev) => {
      const resBadge = ev.result === "success"
        ? '<span class="pill pill-green">Success</span>'
        : ev.result === "pulling"
          ? '<span class="pill pill-blue">Pulling...</span>'
          : ev.result === "canceled"
            ? '<span class="pill pill-gray">Canceled</span>'
            : '<span class="pill pill-red">Error</span>';
      return `
        <tr>
          <td class="mono muted">${fmtTime(ev.timestamp)}</td>
          <td class="mono">${esc(ev.node)}</td>
          <td class="mono">${esc(ev.image)}</td>
          <td>${esc(ev.trigger)}${ev.actor ? " · " + esc(ev.actor) : ""}</td>
          <td class="mono muted">${ev.durationMs ? (ev.durationMs / 1000).toFixed(2) + "s" : "—"}</td>
          <td>${resBadge}${ev.error ? ` <span class="cell-err">${esc(ev.error)}</span>` : ""}</td>
        </tr>
      `;
    }).join("");
  }

  // Event bindings
  $("login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("login-error").classList.add("hidden");
    try {
      const data = await api("/api/v1/auth/login", {
        method: "POST",
        body: JSON.stringify({ username: $("login-user").value, password: $("login-pass").value }),
      });
      $("login-pass").value = "";
      showApp(data.username);
    } catch (err) {
      $("login-error").textContent = err.message;
      $("login-error").classList.remove("hidden");
    }
  });

  $("btn-logout").addEventListener("click", async () => {
    await api("/api/v1/auth/logout", { method: "POST" }).catch(() => {});
    showLogin();
  });

  $("cluster-select").addEventListener("change", (e) => {
    state.activeCluster = e.target.value;
    state.expanded.clear();
    refresh();
  });

  $("btn-sync").addEventListener("click", async () => {
    try {
      await api("/api/v1/reconcile", { method: "POST", body: JSON.stringify({ cluster: state.activeCluster }) });
      showToast("Синхронизация выполнена");
      await refresh();
    } catch (err) {
      showToast(`Ошибка: ${err.message}`);
    }
  });

  $("autorefresh-cb").addEventListener("change", (e) => {
    state.autoRefresh = e.target.checked;
    if (state.autoRefresh) startPolling();
    else stopPolling();
  });

  document.querySelectorAll(".tab").forEach((btn) => {
    btn.addEventListener("click", () => {
      state.activeTab = btn.dataset.tab;
      document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t === btn));
      document.querySelectorAll(".tab-panel").forEach((p) => p.classList.toggle("hidden", p.id !== `tab-${state.activeTab}`));
    });
  });

  ["filter-search", "filter-ns", "filter-status"].forEach((id) => {
    $(id).addEventListener("input", () => state.snapshot && renderWorkloads(state.snapshot));
    $(id).addEventListener("change", () => state.snapshot && renderWorkloads(state.snapshot));
  });

  $("kpi-warnings-card").addEventListener("click", () => {
    document.querySelector('.tab[data-tab="workloads"]').click();
    $("filter-status").value = $("filter-status").value === "warnings" ? "all" : "warnings";
    if (state.snapshot) renderWorkloads(state.snapshot);
  });

  // Boot
  api("/api/v1/auth/me")
    .then((data) => showApp(data.username))
    .catch(() => showLogin());
})();
