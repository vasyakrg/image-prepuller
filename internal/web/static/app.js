(() => {
  const state = {
    user: null,
    // Role from /auth/me or login: "operator" enables pull/sync/cancel controls.
    role: "",
    clusters: [],
    activeCluster: "",
    snapshot: null,
    activeTab: "workloads",
    expanded: new Set(),
    pollTimer: null,
    autoRefresh: true,
    // Open node detail panel: { name, data, error } or null when closed.
    nodeDetail: null,
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

  function fmtDur(ms) {
    if (!ms) return "—";
    const s = ms / 1000;
    return s < 10 ? s.toFixed(2) + "s" : Math.round(s) + "s";
  }

  // Duration is reported by the API once the task finishes; while it is still
  // active we derive a live value from startedAt so the table does not go stale.
  function taskDuration(t) {
    if (t.durationMs) return fmtDur(t.durationMs);
    if (!t.startedAt) return "—";
    const started = new Date(t.startedAt).getTime();
    if (t.state === "running") return fmtDur(Date.now() - started);
    if (t.finishedAt) return fmtDur(new Date(t.finishedAt).getTime() - started);
    return "—";
  }

  function taskStateLabel(st) {
    switch (st) {
      case "queued": return { text: "Queued", cls: "pill-neutral" };
      case "running": return { text: "Running", cls: "pill-blue" };
      case "succeeded": return { text: "Succeeded", cls: "pill-green" };
      case "failed": return { text: "Failed", cls: "pill-red" };
      case "canceled": return { text: "Canceled", cls: "pill-gray" };
      default: return { text: st || "—", cls: "pill-neutral" };
    }
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
    state.role = "";
    $("app-view").classList.add("hidden");
    $("login-view").classList.remove("hidden");
    $("login-user").focus();
  }

  // Only the operator role may run state-changing actions; viewer keeps read-only UI.
  function isOperator() {
    return state.role === "operator";
  }

  function showApp(username, role) {
    state.user = username;
    state.role = role || "";
    $("user-badge").textContent = role ? `${username} · ${role}` : username;
    $("btn-sync").disabled = !isOperator();
    $("btn-sync").title = isOperator() ? "" : "Доступно только роли operator";
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
      const snap = await api(`/api/v1/snapshot${qs}`);
      // An older spoke (or a provider with nothing to list) may send null where
      // the contract promises an array; the render code below relies on arrays.
      snap.workloads = snap.workloads || [];
      snap.nodes = snap.nodes || [];
      snap.tasks = snap.tasks || [];
      snap.activity = snap.activity || [];
      state.snapshot = snap;
      render();
      if (state.nodeDetail) await refreshNodeDetail();
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
    renderTasks(snap);
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
      const empty = snap.workloads.length === 0
        ? "В области действия нет workload'ов. Проверьте, что объекты (Deployment/StatefulSet/DaemonSet) помечены меткой из -workload-selector (в чарте по умолчанию prepull=true)."
        : "Нет сервисов по выбранным фильтрам";
      tbody.innerHTML = `<tr><td colspan="6" class="muted" style="text-align:center;padding:28px">${esc(empty)}</td></tr>`;
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
              data-ns="${esc(w.namespace)}" data-kind="${esc(w.kind)}" data-name="${esc(w.name)}"
              ${isOperator() ? "" : "disabled"} title="${isOperator() ? "" : "Доступно только роли operator"}">
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
            const canPull = isOperator() && ns.state !== "offline" && ns.state !== "pulling";
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

      const selected = state.nodeDetail && state.nodeDetail.name === n.name;
      return `
        <div class="node-card node-card-clickable${selected ? " node-card-active" : ""}"
             data-node="${esc(n.name)}" role="button" tabindex="0"
             aria-pressed="${selected ? "true" : "false"}"
             title="Открыть детали ноды ${esc(n.name)}">
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

    grid.querySelectorAll(".node-card").forEach((card) => {
      const name = card.dataset.node;
      const toggle = () => toggleNodeDetail(name);
      card.addEventListener("click", toggle);
      card.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          toggle();
        }
      });
    });
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
          <td>${resBadge}${ev.error ? ` <span class="cell-err cell-err-clip" title="${esc(ev.error)}">${esc(ev.error)}</span>` : ""}</td>
        </tr>
      `;
    }).join("");
  }

  // --- Pull tasks -------------------------------------------------------
  function clusterQuery() {
    return state.activeCluster ? `?cluster=${encodeURIComponent(state.activeCluster)}` : "";
  }

  function renderTasks(snap) {
    const tasks = snap.tasks || [];
    const queued = tasks.filter((t) => t.state === "queued").length;
    const running = tasks.filter((t) => t.state === "running").length;
    const failed = tasks.filter((t) => t.state === "failed").length;

    $("tab-tasks-count").textContent = String(queued + running);
    $("tasks-summary").textContent =
      `Всего: ${tasks.length}` +
      (queued ? ` · queued ${queued}` : "") +
      (running ? ` · running ${running}` : "") +
      (failed ? ` · failed ${failed}` : "");

    const tbody = $("tasks-tbody");
    if (tasks.length === 0) {
      tbody.innerHTML = '<tr><td colspan="9" class="empty-row">Пока нет pull-задач. Запустите Pull из Workloads или дождитесь auto-триггера.</td></tr>';
      return;
    }

    tbody.innerHTML = tasks.map((t) => {
      const st = taskStateLabel(t.state);
      const isActive = t.state === "queued" || t.state === "running";
      const trigger = t.trigger ? `${t.trigger}${t.actor ? " · " + t.actor : ""}` : (t.actor || "—");
      const created = fmtTime(t.createdAt);
      const cancelBtn = isActive && isOperator()
        ? `<button class="btn btn-secondary btn-xs btn-task-cancel" data-task-id="${esc(t.id)}" title="Отменить задачу ${esc(t.id)}">✕ Отменить</button>`
        : '<span class="muted">—</span>';
      return `
        <tr>
          <td class="mono">${esc(t.id)}</td>
          <td class="mono">${esc(t.image)}</td>
          <td class="mono">${esc(t.node)}</td>
          <td><span class="pill ${st.cls}">${esc(st.text)}</span></td>
          <td class="mono">#${t.attempt ?? 1}</td>
          <td>${esc(trigger)}</td>
          <td class="mono muted">${created} · ${taskDuration(t)}</td>
          <td>${t.error ? `<span class="cell-err cell-err-clip" title="${esc(t.error)}">${esc(t.error)}</span>` : '<span class="muted">—</span>'}</td>
          <td class="right">${cancelBtn}</td>
        </tr>
      `;
    }).join("");

    tbody.querySelectorAll(".btn-task-cancel").forEach((btn) => {
      btn.addEventListener("click", async () => {
        const id = btn.dataset.taskId;
        btn.disabled = true;
        try {
          await api(`/api/v1/pulls/${encodeURIComponent(id)}/cancel${clusterQuery()}`, { method: "POST" });
          showToast(`Задача отменена: ${id}`);
          await refresh();
        } catch (err) {
          btn.disabled = false;
          showToast(`Не удалось отменить задачу: ${err.message}`);
        }
      });
    });
  }

  // --- Node detail panel ------------------------------------------------
  function toggleNodeDetail(name) {
    if (state.nodeDetail && state.nodeDetail.name === name) {
      closeNodeDetail();
      return;
    }
    state.nodeDetail = { name, data: null, error: "" };
    if (state.snapshot) renderNodes(state.snapshot);
    renderNodeDetail();
    refreshNodeDetail();
  }

  function closeNodeDetail() {
    state.nodeDetail = null;
    renderNodeDetail();
    if (state.snapshot) renderNodes(state.snapshot);
  }

  async function refreshNodeDetail() {
    const nd = state.nodeDetail;
    if (!nd) return;
    try {
      const data = await api(`/api/v1/nodes/${encodeURIComponent(nd.name)}/images${clusterQuery()}`);
      if (!state.nodeDetail || state.nodeDetail.name !== nd.name) return;
      state.nodeDetail.data = data;
      state.nodeDetail.error = "";
      renderNodeDetail();
    } catch (err) {
      if (!state.nodeDetail || state.nodeDetail.name !== nd.name) return;
      if (err.message === "unauthorized") return;
      state.nodeDetail.error = err.message;
      renderNodeDetail();
    }
  }

  function renderNodeDetail() {
    const el = $("node-detail");
    const nd = state.nodeDetail;
    if (!nd) {
      el.classList.add("hidden");
      el.innerHTML = "";
      return;
    }
    el.classList.remove("hidden");

    const head = `
      <div class="node-detail-head">
        <div>
          <div class="muted node-detail-eyebrow">Node detail</div>
          <strong class="mono" style="font-size:15px">${esc(nd.name)}</strong>
        </div>
        <button id="btn-node-detail-close" class="btn btn-ghost btn-xs" title="Закрыть панель (Esc)">✕ Закрыть</button>
      </div>
    `;

    if (!nd.data) {
      el.innerHTML = head + (nd.error
        ? `<div class="error-banner">${esc(nd.error)}</div>`
        : '<div class="muted" style="font-size:12.5px">Загрузка данных ноды…</div>');
      el.querySelector("#btn-node-detail-close").addEventListener("click", closeNodeDetail);
      return;
    }

    const d = nd.data;
    const pct = d.imageFsTotal > 0 ? (d.imageFsUsed / d.imageFsTotal) * 100 : 0;
    const snapNode = ((state.snapshot && state.snapshot.nodes) || []).find((n) => n.name === nd.name);
    let barColor = "var(--green)";
    if (snapNode && pct >= snapNode.kubeletGcThreshold * 100) barColor = "var(--red)";
    else if (snapNode && pct >= snapNode.diskPauseThreshold * 100) barColor = "var(--purple)";
    else if (pct >= 70) barColor = "var(--amber)";

    const badges = [
      d.ready ? '<span class="pill pill-green">Ready</span>' : '<span class="pill pill-red">NotReady</span>',
      d.agentUp ? '<span class="pill pill-green">Agent UP</span>' : '<span class="pill pill-red">Agent DOWN</span>',
      d.diskPaused ? '<span class="pill pill-purple">Disk Paused</span>' : "",
    ].filter(Boolean).join(" ");

    const cached = d.cachedImages || [];
    const cachedHtml = cached.length
      ? cached.map((ci) => `<div class="cached-item"><span class="mono" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(ci.image)}</span><span class="muted">${fmtBytes(ci.sizeBytes)}</span></div>`).join("")
      : '<div class="muted" style="font-size:12px">Нет кэшированных образов</div>';

    const conditions = d.conditions || [];
    const conditionsHtml = conditions.length
      ? conditions.map((c) => `
          <div class="condition-item">
            <span class="pill ${c.status === "True" ? "pill-green" : "pill-red"}">${esc(c.type)}: ${esc(c.status)}</span>
            <span class="muted condition-detail">${esc(c.reason || "")}${c.message ? " · " + esc(c.message) : ""}</span>
          </div>
        `).join("")
      : '<div class="muted" style="font-size:12px">Условий нет</div>';

    el.innerHTML = head + `
      <div class="node-detail-badges">${badges}</div>
      <div class="node-detail-meta">
        <div class="node-detail-stat">
          <div class="node-detail-label">ImageFS used / capacity</div>
          <div class="mono">${fmtBytes(d.imageFsUsed)} / ${fmtBytes(d.imageFsTotal)} (${pct.toFixed(1)}%)</div>
        </div>
        <div class="node-detail-stat">
          <div class="node-detail-label">Disk paused</div>
          <div class="mono">${d.diskPaused ? "yes" : "no"}</div>
        </div>
        <div class="node-detail-stat">
          <div class="node-detail-label">Agent version</div>
          <div class="mono">${esc(d.agentVersion || "—")}</div>
        </div>
        <div class="node-detail-stat">
          <div class="node-detail-label">Last seen</div>
          <div class="mono">${fmtTime(d.lastSeen)}</div>
        </div>
      </div>
      <div class="disk-bar-wrap" title="ImageFS: ${pct.toFixed(1)}%">
        <div class="disk-bar-fill" style="width:${Math.min(pct, 100).toFixed(1)}%;background:${barColor}"></div>
      </div>
      <div class="node-detail-section-title">Cached images (${cached.length})</div>
      <div class="cached-list node-detail-cached">${cachedHtml}</div>
      <div class="node-detail-section-title">Conditions</div>
      <div class="conditions-list">${conditionsHtml}</div>
    `;
    el.querySelector("#btn-node-detail-close").addEventListener("click", closeNodeDetail);
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
      showApp(data.username, data.role);
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
    closeNodeDetail();
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

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && state.nodeDetail) closeNodeDetail();
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
    .then((data) => showApp(data.username, data.role))
    .catch(() => showLogin());
})();
