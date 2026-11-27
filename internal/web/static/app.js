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
    // Free-text filter over the cached images of the open node detail panel.
    nodeImageQuery: "",
    // Pull-tasks page: server-side pagination over the hub history store.
    taskPage: { limit: 50, offset: 0, total: 0, active: 0, state: "all", source: "" },
    taskPollTimer: null,
    // Last successful task page payload, so a re-render does not lose it.
    tasks: [],

    // Stats tab: read-only view over the optional PostgreSQL statistics store.
    // mode is "loading" | "disabled" | "error" | "ok"; nothing but
    // /stats/status is queried until the backend reports enabled+ok.
    stats: {
      info: null,
      mode: "loading",
      error: "",
      window: "24h",
      cluster: "",
      loaded: false,
      loading: false,
      pending: false,
      loadedAt: 0,
      timer: null,
      req: { overview: 0, nodes: 0, images: 0, gc: 0, repulls: 0, tasks: 0 },
      overview: null,
      overviewError: "",
      nodes: [],
      nodesError: "",
      rows: { images: [], gc: [], repulls: [], tasks: [] },
      errorFor: { images: "", gc: "", repulls: "", tasks: "" },
      pages: {
        images: { limit: 50, offset: 0, total: 0 },
        gc: { limit: 50, offset: 0, total: 0 },
        repulls: { limit: 50, offset: 0, total: 0 },
        tasks: { limit: 50, offset: 0, total: 0 },
      },
      filters: {
        images: { q: "", state: "" },
        tasks: { q: "", state: "" },
      },
    },
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

  // Condition types where "True" is the healthy state; for the pressure types
  // it is the opposite, so the pill colour must follow the type, not the value.
  const pressureConditions = new Set(["MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"]);

  // conditionTone returns "ok" | "bad" | "unknown" for one node condition.
  function conditionTone(c) {
    if (c.status !== "True" && c.status !== "False") return "unknown";
    const trueIsGood = !pressureConditions.has(c.type);
    const isTrue = c.status === "True";
    return (trueIsGood ? isTrue : !isTrue) ? "ok" : "bad";
  }

  // diskUsage renders used/capacity for one node. CRI ImageFsInfo may report the
  // used bytes without any capacity (containerd on Talos does), and the provider
  // then falls back to the kubelet ephemeral-storage capacity and flags it as an
  // estimate - a "41.5 GB / 0 B (0.0%)" reading is worse than no number at all.
  function diskUsage(n) {
    const used = fmtBytes(n.imageFsUsedBytes);
    if (!n.imageFsCapacityBytes) {
      return { text: `${used} · ёмкость неизвестна`, pct: null };
    }
    const pct = (n.imageFsUsedBytes / n.imageFsCapacityBytes) * 100;
    const cap = fmtBytes(n.imageFsCapacityBytes);
    return {
      text: n.imageFsCapacityEstimated
        ? `${used} / ~${cap} (${pct.toFixed(1)}%)`
        : `${used} / ${cap} (${pct.toFixed(1)}%)`,
      pct,
    };
  }

  function diskBarColor(pct, node) {
    if (pct === null) return "transparent";
    if (node && pct >= node.kubeletGcThreshold * 100) return "var(--red)";
    if (node && pct >= node.diskPauseThreshold * 100) return "var(--purple)";
    if (pct >= 70) return "var(--amber)";
    return "var(--green)";
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
  // kind "error" paints the toast red (inline, no stylesheet change) for the
  // state-changing actions that report "nothing happened".
  function showToast(msg, kind = "info") {
    const el = $("toast");
    el.textContent = msg;
    el.style.borderColor = kind === "error" ? "var(--red)" : "";
    el.style.color = kind === "error" ? "var(--red)" : "";
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
    // Drop the stats view too: the next login must not reuse a snapshot that
    // was fetched under the previous session (and its cookie).
    state.stats.info = null;
    state.stats.mode = "loading";
    state.stats.error = "";
    state.stats.loaded = false;
    state.stats.loadedAt = 0;
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
    // Pull tasks are shorter-lived than the 5 s snapshot poll: a pull that starts
    // and finishes between two snapshots stayed invisible in the tab badge. The
    // dedicated, cheaper endpoint is polled every 2 s instead.
    pollTaskBadge();
    state.taskPollTimer = setInterval(() => {
      if (!state.user || !state.autoRefresh) return;
      pollTaskBadge();
      if (state.activeTab === "tasks") refreshTaskPage({ silent: true });
    }, 2000);
  }

  function stopPolling() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
    if (state.taskPollTimer) {
      clearInterval(state.taskPollTimer);
      state.taskPollTimer = null;
    }
    // The stats feed changes once per collection interval, not every 5 s; its
    // own timer must not keep querying once polling is off or the tab is left.
    stopStatsTimer();
  }

  // Active tasks plus the ones finished within this window keep the menu badge
  // non-zero right after a pull, so "Pull Tasks 0" no longer hides short pulls.
  const TASK_RECENT_MS = 30000;

  async function pollTaskBadge() {
    try {
      const data = await api(`/api/v1/pull-tasks?limit=20&offset=0${state.activeCluster ? "&cluster=" + encodeURIComponent(state.activeCluster) : ""}`);
      const tasks = data.tasks || [];
      const now = Date.now();
      const recent = tasks.filter((t) => {
        if (t.state === "queued" || t.state === "running") return false;
        if (!t.finishedAt) return false;
        return now - new Date(t.finishedAt).getTime() <= TASK_RECENT_MS;
      }).length;
      updateTaskBadge(data.active || 0, recent);
    } catch (err) {
      if (err.message !== "unauthorized") updateTaskBadge(0, 0);
    }
  }

  function updateTaskBadge(active, recent) {
    const el = $("tab-tasks-count");
    const shown = active + recent;
    el.textContent = String(shown);
    el.classList.toggle("tab-count-hot", active > 0);
    el.title = active > 0
      ? `${active} активных · ${recent} завершено за последние 30 с`
      : `${recent} завершено за последние 30 с`;
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
      if (state.tasks.length === 0) await refreshTaskPage({ silent: true });
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

    // Coverage: presentSlots / totalSlots is "image x node" - the share of
    // (image, node) pairs that already have the image in the node cache.
    const missingSlots = Math.max(s.totalSlots - s.presentSlots, 0);
    $("kpi-coverage").textContent = `${s.coveragePercent.toFixed(1)}%`;
    $("kpi-coverage-sub").textContent =
      `${s.presentSlots} из ${s.totalSlots} слотов (образ × нода)` +
      (missingSlots > 0 ? ` · ${missingSlots} не прогрето` : " · всё прогрето");

    // Agent state is not in Summary: the node list knows which agents answer.
    const nodes = snap.nodes;
    const agentsUp = nodes.filter((n) => n.agentUp).length;
    const agentsDown = nodes.length - agentsUp;
    $("kpi-agents").textContent = `${agentsUp} / ${nodes.length}`;
    $("kpi-agents").style.color = agentsDown > 0 ? "var(--amber)" : "var(--green)";
    $("kpi-agents-sub").textContent = nodes.length === 0
      ? "нод в области действия нет"
      : agentsDown > 0
        ? `${agentsDown} без агента · ${s.readyWorkers}/${s.totalWorkers} Ready`
        : `все агенты на связи · ${s.readyWorkers}/${s.totalWorkers} Ready`;

    // ImageFS: total used across nodes plus the worst-filled node, so an operator
    // sees disk pressure without expanding every node card.
    const diskUsed = nodes.reduce((acc, n) => acc + (n.imageFsUsedBytes || 0), 0);
    const withCapacity = nodes.filter((n) => n.imageFsCapacityBytes > 0);
    const estimatedCapacity = withCapacity.some((n) => n.imageFsCapacityEstimated);
    let worst = null;
    for (const n of withCapacity) {
      const pct = (n.imageFsUsedBytes / n.imageFsCapacityBytes) * 100;
      if (!worst || pct > worst.pct) worst = { node: n, pct };
    }
    $("kpi-disk").textContent = fmtBytes(diskUsed);
    if (!worst) {
      $("kpi-disk").style.color = "var(--text)";
      $("kpi-disk-sub").textContent = nodes.length
        ? "ёмкость ImageFS неизвестна (used-only CRI, например Talos)"
        : "—";
    } else {
      const fill = worst.pct;
      $("kpi-disk").style.color = fill >= 85 ? "var(--red)" : fill >= 70 ? "var(--amber)" : "var(--text)";
      $("kpi-disk-sub").textContent =
        `заполнено ${fill.toFixed(1)}% · худшая ${worst.node.name.replace(/^talos-/, "")}${estimatedCapacity ? " · ёмкость оценочная" : ""}`;
    }

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
    renderTasks();
    renderActivity(snap);
  }

  // A policy warning is about one token of the image description: the ":latest"
  // tag or the "Always" pull policy. Highlighting that token (amber) is what
  // makes the warning actionable in the Workloads table.
  function imageWarnReasons(img) {
    const reasons = new Set();
    for (const v of img.policyViolations || []) reasons.add(v.reason);
    return reasons;
  }

  function imageRefHtml(img) {
    const reasons = imageWarnReasons(img);
    const tag = (img.ref && img.ref.tag) || "";
    if (reasons.has("latest_tag") && tag === "latest" && String(img.image).endsWith(":latest")) {
      const base = String(img.image).slice(0, -":latest".length);
      return `${esc(base)}<span class="warn-token" title="Тег :latest не фиксирует версию: kubelet тянет образ заново, а прогретый кэш не гарантирует нужный диджест">:latest</span>`;
    }
    return esc(img.image);
  }

  function imagePolicyHtml(img) {
    const reasons = imageWarnReasons(img);
    if (!reasons.has("pull_policy_always")) return "";
    const policy = img.pullPolicy || "Always";
    return ` · <span class="warn-token" title="imagePullPolicy: ${esc(policy)} — kubelet тянет образ при каждом старте пода, прогрев кэша не помогает">policy: ${esc(policy)}</span>`;
  }

  function imagePolicyLabelHtml(img) {
    const policy = esc(img.pullPolicy || "default");
    if (!imageWarnReasons(img).has("pull_policy_always")) return policy;
    return `<span class="warn-token" title="imagePullPolicy заставляет kubelet тянуть образ при каждом старте пода">${policy}</span>`;
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
        `<div class="mono img-line">${i.isInit ? '<span class="pill pill-neutral" style="margin-right:4px">init</span>' : ""}${imageRefHtml(i)}${imagePolicyHtml(i)}</div>`
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
                <div class="mono">${imageRefHtml(img)}</div>
                <div class="cell-meta">container: ${esc(img.containerName)} · policy: ${imagePolicyLabelHtml(img)}</div>
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
          const triggered = Number(res.triggered) || 0;
          if (triggered === 0) {
            showToast("Прогрев не запущен: ни одной задачи не создано", "error");
          } else {
            showToast(`Прогрев запущен: ${triggered} задач`);
          }
          // Tasks are created asynchronously; refresh them right away instead of
          // waiting for the next 2 s task poll tick, so Pull All shows results.
          await Promise.all([refreshTaskPage({ silent: true }), pollTaskBadge()]);
          await refresh();
        } catch (err) {
          showToast(`Ошибка: ${err.message}`, "error");
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
      const du = diskUsage(n);
      const pct = du.pct;
      const barColor = diskBarColor(pct, n);
      // CRI ImageFsInfo may have no capacity at all (Talos): do not draw a 0%
      // bar that reads as "empty disk", say the capacity is unknown instead.
      const barTip = n.imageFsCapacityEstimated
        ? "Ёмкость взята из ephemeral-storage ноды (CRI отдал только used)"
        : "Белые метки: порог паузы авто-пула и порог Kubelet GC";
      const barHtml = pct === null
        ? `<div class="disk-bar-wrap disk-bar-unknown" title="CRI ImageFsInfo не сообщает ёмкость, процент заполнения неизвестен"><span class="disk-bar-unknown-label">ёмкость неизвестна</span></div>`
        : `<div class="disk-bar-wrap" title="${esc(barTip)}">
            <div class="disk-bar-fill" style="width:${Math.min(pct, 100).toFixed(1)}%;background:${barColor}"></div>
            <div class="disk-marker" style="left:${(n.diskPauseThreshold * 100).toFixed(0)}%"></div>
            <div class="disk-marker" style="left:${(n.kubeletGcThreshold * 100).toFixed(0)}%"></div>
          </div>`;

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
          <div style="display:flex;justify-content:space-between;font-size:12px;gap:8px">
            <span class="muted">EPHEMERAL / ImageFS${n.imageFsCapacityEstimated ? ' <span class="pill pill-neutral" title="CRI вернул только used; ёмкость — ephemeral-storage из Node.status">est.</span>' : ""}</span>
            <span class="mono" style="text-align:right">${du.text}</span>
          </div>
          ${barHtml}
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

  // refreshTaskPage reads one page of the hub pull-task history. Providers keep
  // only the last ~200 tasks in memory, so the tab paginates over the hub store
  // (which can persist to disk) instead of rendering a single snapshot array.
  async function refreshTaskPage(opts = {}) {
    const page = state.taskPage;
    const params = new URLSearchParams({ limit: String(page.limit), offset: String(page.offset), state: page.state });
    if (state.activeCluster) params.set("cluster", state.activeCluster);
    try {
      const data = await api(`/api/v1/pull-tasks?${params.toString()}`);
      state.tasks = data.tasks || [];
      page.total = data.total || 0;
      page.active = data.active || 0;
      page.source = data.source || "";
      renderTasks();
      $("tasks-err").classList.add("hidden");
    } catch (err) {
      if (err.message === "unauthorized") return;
      if (!opts.silent) {
        $("tasks-err").textContent = `Не удалось загрузить pull-задачи: ${err.message}`;
        $("tasks-err").classList.remove("hidden");
      }
    }
  }

  function renderTasks() {
    const page = state.taskPage;
    const tasks = state.tasks;
    const from = page.total === 0 ? 0 : page.offset + 1;
    const to = page.offset + tasks.length;
    const totalPages = Math.max(Math.ceil(page.total / page.limit), 1);
    const currentPage = Math.min(Math.floor(page.offset / page.limit) + 1, totalPages);

    $("tasks-summary").textContent =
      `Всего: ${page.total}` +
      (page.active ? ` · активных ${page.active}` : "");
    // Source tells the operator whether the list is the hub's bounded history
    // (survives a container restart) or the provider snapshot (memory only).
    const sourceEl = $("tasks-source");
    sourceEl.textContent = page.source === "snapshot" ? "источник: snapshot" : "источник: история hub";
    sourceEl.title = page.source === "snapshot"
      ? "Провайдер отдаёт только последние ~200 задач из памяти; hub-история недоступна"
      : "Ограниченная история hub'а: переживает рестарт контейнера при смонтированном каталоге";
    $("tasks-page-info").textContent = page.total === 0
      ? "0"
      : `${from}–${to} из ${page.total} · стр. ${currentPage}/${totalPages}`;
    $("tasks-prev").disabled = page.offset <= 0;
    $("tasks-next").disabled = page.offset + page.limit >= page.total;

    const tbody = $("tasks-tbody");
    if (tasks.length === 0) {
      const filterNote = page.state !== "all" ? " по выбранному фильтру" : "";
      tbody.innerHTML = `<tr><td colspan="9" class="empty-row">Пока нет pull-задач${filterNote}. Запустите Pull из Workloads или дождитесь auto-триггера.</td></tr>`;
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
          await refreshTaskPage({ silent: true });
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

  // applyNodeImageFilter hides cached images of the open node that do not match
  // the search box. It filters the DOM in place so typing never loses focus or
  // triggers a panel rebuild from the 5 s poll.
  function applyNodeImageFilter() {
    const el = $("node-detail");
    if (!el) return;
    const q = state.nodeImageQuery.trim().toLowerCase();
    let shown = 0;
    let all = 0;
    el.querySelectorAll(".node-detail-cached .cached-item").forEach((item) => {
      all++;
      const hit = !q || String(item.dataset.image || "").toLowerCase().includes(q);
      item.classList.toggle("hidden", !hit);
      if (hit) shown++;
    });
    const counter = el.querySelector("#node-image-count");
    if (counter) counter.textContent = q ? `${shown} из ${all}` : String(all);
    const empty = el.querySelector("#node-image-empty");
    if (empty) empty.classList.toggle("hidden", !q || shown > 0);
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

    // A full rebuild would drop the caret and the selection of the image search
    // box, so skip renders while it is focused; its blur handler re-renders.
    if (document.activeElement && document.activeElement.id === "node-image-filter") return;

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
    const snapNode = ((state.snapshot && state.snapshot.nodes) || []).find((n) => n.name === nd.name);
    const du = diskUsage({
      imageFsUsedBytes: d.imageFsUsed,
      imageFsCapacityBytes: d.imageFsTotal,
      imageFsCapacityEstimated: d.imageFsCapacityEstimated,
    });
    const barColor = diskBarColor(du.pct, snapNode);
    const diskNote = du.pct === null
      ? "CRI ImageFsInfo вернул только used: ёмкость ImageFS неизвестна, процент не считается"
      : d.imageFsCapacityEstimated
        ? "Ёмкость взята из ephemeral-storage ноды (оценка)"
        : "";

    const badges = [
      d.ready ? '<span class="pill pill-green">Ready</span>' : '<span class="pill pill-red">NotReady</span>',
      d.agentUp ? '<span class="pill pill-green">Agent UP</span>' : '<span class="pill pill-red">Agent DOWN</span>',
      d.diskPaused ? '<span class="pill pill-purple">Disk Paused</span>' : "",
    ].filter(Boolean).join(" ");

    const cached = d.cachedImages || [];
    const cachedHtml = cached.length
      ? cached.map((ci) => `<div class="cached-item" data-image="${esc(ci.image)}"><span class="mono" style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(ci.image)}</span><span class="muted">${fmtBytes(ci.sizeBytes)}</span></div>`).join("")
      : '<div class="muted" style="font-size:12px">Нет кэшированных образов</div>';

    // Conditions are not uniformly "True is good": for pressure conditions the
    // healthy value is False, so colour follows the type, not the raw status.
    const conditions = d.conditions || [];
    const conditionsHtml = conditions.length
      ? conditions.map((c) => {
          const tone = conditionTone(c);
          const cls = tone === "ok" ? "pill-green" : tone === "bad" ? "pill-red" : "pill-gray";
          const title = pressureConditions.has(c.type)
            ? "Для этого условия здоровое состояние — False"
            : "Для этого условия здоровое состояние — True";
          return `<div class="condition-item">
            <span class="pill ${cls}" title="${esc(title)}">${esc(c.type)}: ${esc(c.status)}</span>
            <span class="muted condition-detail">${esc(c.reason || "")}${c.message ? " · " + esc(c.message) : ""}</span>
          </div>`;
        }).join("")
      : '<div class="muted" style="font-size:12px">Условий нет</div>';

    el.innerHTML = head + `
      <div class="node-detail-badges">${badges}</div>
      <div class="node-detail-meta">
        <div class="node-detail-stat">
          <div class="node-detail-label">ImageFS used / capacity</div>
          <div class="mono">${du.text}</div>
          ${diskNote ? `<div class="muted" style="font-size:11px">${esc(diskNote)}</div>` : ""}
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
      ${du.pct === null
        ? '<div class="disk-bar-wrap disk-bar-unknown"><span class="disk-bar-unknown-label">ёмкость неизвестна</span></div>'
        : `<div class="disk-bar-wrap" title="ImageFS: ${du.pct.toFixed(1)}%">
             <div class="disk-bar-fill" style="width:${Math.min(du.pct, 100).toFixed(1)}%;background:${barColor}"></div>
           </div>`}
      <div class="node-detail-section-title">
        Cached images <span id="node-image-count" class="muted">${cached.length}</span>
      </div>
      <input id="node-image-filter" class="input node-image-filter" type="search"
        placeholder="Поиск по образу на ноде (имя, тег, digest)..."
        value="${esc(state.nodeImageQuery)}" autocomplete="off" />
      <div id="node-image-empty" class="muted node-image-empty hidden">Ничего не найдено</div>
      <div class="cached-list node-detail-cached">${cachedHtml}</div>
      <div class="node-detail-section-title">Conditions</div>
      <div class="conditions-list">${conditionsHtml}</div>
    `;
    el.querySelector("#btn-node-detail-close").addEventListener("click", closeNodeDetail);

    const filterEl = el.querySelector("#node-image-filter");
    if (filterEl) {
      filterEl.addEventListener("input", () => {
        state.nodeImageQuery = filterEl.value;
        applyNodeImageFilter();
      });
      filterEl.addEventListener("blur", () => {
        if (state.nodeDetail && !state.nodeDetail.error) renderNodeDetail();
      });
      applyNodeImageFilter();
    }
  }

  // --- Stats ------------------------------------------------------------
  // Read-only view over the optional PostgreSQL statistics store. The backend
  // is best-effort: /api/v1/stats/status reports whether collection is enabled
  // and whether the DB answers, and no other stats endpoint is queried unless
  // both are true. Each list keeps its own pagination and error banner, so one
  // failing query does not take the whole tab down.
  const STATS_TICK_MS = 30000;
  // Collection runs every ~30 s: re-reading the tab faster only burns queries.
  const STATS_MAX_AGE_MS = 25000;
  const STATS_LIST_NAMES = ["images", "gc", "repulls", "tasks"];
  const STATS_COLS = { images: 7, gc: 6, repulls: 6, tasks: 8 };

  function statsNum(v, digits = 0) {
    const n = Number(v);
    if (!isFinite(n)) return "—";
    return digits > 0 ? n.toFixed(digits) : String(Math.round(n));
  }

  function fmtRetention(sec) {
    const n = Number(sec);
    if (!isFinite(n) || n <= 0) return "—";
    const days = n / 86400;
    return days >= 1 ? `${Math.round(days)} дн.` : `${Math.max(Math.round(n / 3600), 1)} ч`;
  }

  // The stats API reports durations in seconds; a lifetime can be days long, so
  // seconds are upgraded to minutes/hours/days instead of printing "86400s".
  // GC windowSeconds and short pull lags stay in seconds.
  function fmtSeconds(sec) {
    if (sec === null || sec === undefined || sec === "") return "—";
    const n = Number(sec);
    if (!isFinite(n) || n < 0) return "—";
    if (n < 0.005) return "0s";
    if (n < 90) return fmtDur(n * 1000);
    if (n < 5400) return (n < 600 ? (n / 60).toFixed(1) : String(Math.round(n / 60))) + " мин";
    if (n < 172800) return (n < 36000 ? (n / 3600).toFixed(1) : String(Math.round(n / 3600))) + " ч";
    return (n < 864000 ? (n / 86400).toFixed(1) : String(Math.round(n / 86400))) + " дн.";
  }

  // Stats timestamps span up to 90 days, where a bare "13:59:49" is ambiguous:
  // the date is added for anything that is not today.
  function fmtStamp(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "—";
    const now = new Date();
    const sameDay = d.getFullYear() === now.getFullYear() &&
      d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    return sameDay
      ? d.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit", second: "2-digit" })
      : d.toLocaleString("ru-RU", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function statsText(v) {
    return v === null || v === undefined || v === "" ? "—" : esc(v);
  }

  // Every stats request carries a sequence number: a slow response that lost
  // the race (window switch, page click, filter change) must not overwrite the
  // fresher rows of the request that was issued after it.
  function statsNextReq(name) {
    const st = state.stats;
    st.req[name] = (st.req[name] || 0) + 1;
    return st.req[name];
  }

  function statsReqCurrent(name, seq) {
    return state.stats.req[name] === seq;
  }

  function statsListParams(name) {
    const st = state.stats;
    const page = st.pages[name];
    const p = new URLSearchParams({ window: st.window, limit: String(page.limit), offset: String(page.offset) });
    if (st.cluster) p.set("cluster", st.cluster);
    if (name === "images") {
      if (st.filters.images.q) p.set("q", st.filters.images.q);
      if (st.filters.images.state) p.set("state", st.filters.images.state);
    }
    if (name === "tasks") {
      if (st.filters.tasks.q) p.set("q", st.filters.tasks.q);
      if (st.filters.tasks.state) p.set("state", st.filters.tasks.state);
    }
    return p.toString();
  }

  // The KPI header and the nodes table take the same cluster filter as the
  // lists: selecting a cluster must move every card on the tab, not only the
  // tables below them.
  function statsClusterQuery() {
    return state.stats.cluster ? "cluster=" + encodeURIComponent(state.stats.cluster) : "";
  }

  async function loadStatsOverview() {
    const st = state.stats;
    if (st.mode !== "ok") return;
    const seq = statsNextReq("overview");
    const cluster = statsClusterQuery();
    try {
      const data = await api(`/api/v1/stats/overview?window=${encodeURIComponent(st.window)}` +
        (cluster ? "&" + cluster : ""));
      if (!statsReqCurrent("overview", seq)) return;
      st.overview = data || null;
      st.overviewError = "";
    } catch (err) {
      if (err.message === "unauthorized" || !statsReqCurrent("overview", seq)) return;
      st.overview = null;
      st.overviewError = err.message;
    }
    renderStatsOverview();
  }

  async function loadStatsNodes() {
    const st = state.stats;
    if (st.mode !== "ok") return;
    const seq = statsNextReq("nodes");
    const cluster = statsClusterQuery();
    try {
      const data = await api("/api/v1/stats/nodes" + (cluster ? "?" + cluster : ""));
      if (!statsReqCurrent("nodes", seq)) return;
      st.nodes = data.rows || [];
      st.nodesError = "";
    } catch (err) {
      if (err.message === "unauthorized" || !statsReqCurrent("nodes", seq)) return;
      st.nodes = [];
      st.nodesError = err.message;
    }
    renderStatsNodes();
  }

  async function loadStatsList(name) {
    const st = state.stats;
    if (st.mode !== "ok") return;
    const seq = statsNextReq(name);
    try {
      const data = await api(`/api/v1/stats/${name}?${statsListParams(name)}`);
      if (!statsReqCurrent(name, seq)) return;
      const page = st.pages[name];
      st.rows[name] = data.rows || [];
      page.total = Number(data.total) || 0;
      // limit stays client-side: the server echoes what was requested (or clamps
      // it to 200), and adopting a clamped value would shift the next page.
      page.offset = Number(data.offset) || 0;
      st.errorFor[name] = "";
    } catch (err) {
      if (err.message === "unauthorized" || !statsReqCurrent(name, seq)) return;
      st.rows[name] = [];
      st.errorFor[name] = err.message;
    }
    renderStatsList(name);
  }

  // loadStats is the only entry point that touches /stats/status. While it is
  // in flight further reloads are folded into one pending rerun, so a quick
  // window/cluster switch cannot stack parallel status+list requests.
  async function loadStats() {
    const st = state.stats;
    if (!state.user) return;
    if (st.loading) {
      st.pending = true;
      return;
    }
    st.loading = true;
    renderStatsToolbar();
    try {
      const info = await api("/api/v1/stats/status");
      st.info = info || {};
      st.error = "";
      if (!st.info.enabled) {
        st.mode = "disabled";
        st.loaded = true;
        st.loadedAt = Date.now();
        return;
      }
      if (!st.info.ok) {
        st.mode = "error";
        st.error = st.info.error || "неизвестная ошибка";
        st.loaded = false;
        return;
      }
      st.mode = "ok";
      renderStatsClusterSelect();
      await Promise.all([
        loadStatsOverview(),
        loadStatsNodes(),
        loadStatsList("images"),
        loadStatsList("gc"),
        loadStatsList("repulls"),
        loadStatsList("tasks"),
      ]);
      st.loaded = true;
      st.loadedAt = Date.now();
    } catch (err) {
      if (err.message === "unauthorized") return;
      st.mode = "error";
      st.error = err.message;
      st.loaded = false;
    } finally {
      st.loading = false;
      renderStats();
      if (st.pending) {
        st.pending = false;
        setTimeout(loadStats, 0);
      }
    }
  }

  function resetStatsOffsets() {
    STATS_LIST_NAMES.forEach((name) => { state.stats.pages[name].offset = 0; });
  }

  function reloadStats(opts = {}) {
    if (opts.resetPages) resetStatsOffsets();
    state.stats.loaded = false;
    loadStats();
  }

  // The feed changes once per collection interval, so the tab reloads on
  // activation (when stale) and on a slow 30 s tick that dies with the tab.
  function activateStatsTab() {
    const st = state.stats;
    startStatsTimer();
    if (!st.loaded || Date.now() - st.loadedAt > STATS_MAX_AGE_MS) loadStats();
    else renderStats();
  }

  function startStatsTimer() {
    stopStatsTimer();
    state.stats.timer = setInterval(() => {
      if (state.activeTab !== "stats" || !state.user || !state.autoRefresh || document.hidden) return;
      loadStats();
    }, STATS_TICK_MS);
  }

  function stopStatsTimer() {
    if (state.stats.timer) {
      clearInterval(state.stats.timer);
      state.stats.timer = null;
    }
  }

  function renderStats() {
    const st = state.stats;
    const mode = st.mode || "loading";
    $("stats-loading").classList.toggle("hidden", mode !== "loading");
    $("stats-disabled").classList.toggle("hidden", mode !== "disabled");
    $("stats-error-block").classList.toggle("hidden", mode !== "error");
    $("stats-content").classList.toggle("hidden", mode !== "ok");
    if (mode === "error") {
      // The backend already sends a ready-to-show Russian sentence ("база
      // статистики недоступна"); prefixing it again would read as a stutter.
      const reason = st.error || "неизвестная ошибка";
      const duplicate = /^база статистики/i.test(reason);
      $("stats-error-msg").textContent = duplicate
        ? reason.charAt(0).toUpperCase() + reason.slice(1)
        : "База статистики недоступна: " + reason;
    }
    // Without a working backend there are no clusters to pick: a stale option
    // list from the last successful load would offer a filter that does nothing.
    if (mode === "disabled" || mode === "error") {
      const sel = $("stats-cluster");
      sel.innerHTML = `<option value="">Все кластеры</option>`;
      sel.value = "";
      sel.disabled = true;
    }
    renderStatsToolbar();
    if (mode !== "ok") return;
    renderStatsOverview();
    renderStatsNodes();
    STATS_LIST_NAMES.forEach(renderStatsList);
  }

  function renderStatsToolbar() {
    const st = state.stats;
    const win = $("stats-window");
    if (win.value !== st.window) win.value = st.window;
    const btn = $("stats-refresh");
    btn.disabled = st.loading;
    btn.textContent = st.loading ? "↻ Обновление…" : "↻ Обновить";
    const updated = $("stats-updated");
    updated.textContent = st.loadedAt
      ? "Обновлено " + fmtStamp(new Date(st.loadedAt).toISOString())
      : "Обновлено —";
    updated.title = st.loadedAt ? "Время последнего успешного чтения статистики" : "Статистика ещё не загружена";
    const info = st.info;
    const collect = $("stats-collect");
    if (info && info.enabled && info.ok) {
      const every = Number(info.intervalSeconds);
      collect.textContent = "сбор каждые " + (isFinite(every) && every > 0 ? Math.round(every) + "s" : "—") +
        " · хранение " + fmtRetention(info.retentionSeconds);
      collect.title = "Подключено: " + (info.connectedAt ? fmtStamp(info.connectedAt) : "—") +
        " · проверено: " + (info.checkedAt ? fmtStamp(info.checkedAt) : "—");
    } else {
      collect.textContent = "";
      collect.title = "";
    }
  }

  function renderStatsClusterSelect() {
    const st = state.stats;
    const sel = $("stats-cluster");
    const clusters = (st.info && st.info.clusters) || [];
    if (clusters.length === 0 || (st.cluster && clusters.indexOf(st.cluster) === -1)) st.cluster = "";
    sel.innerHTML = `<option value="">Все кластеры</option>` +
      clusters.map((c) => `<option value="${esc(c)}">${esc(c)}</option>`).join("");
    sel.value = st.cluster;
    sel.disabled = clusters.length === 0;
    sel.title = clusters.length
      ? "Показать KPI и списки только по выбранному кластеру."
      : "Backend не вернул ни одного кластера статистики";
  }

  function renderStatsOverview() {
    const st = state.stats;
    const errEl = $("stats-overview-err");
    errEl.textContent = st.overviewError ? "Не удалось загрузить KPI: " + st.overviewError : "";
    errEl.classList.toggle("hidden", !st.overviewError);
    const o = st.overview;
    const keys = ["nodes", "images", "gc", "repulls", "tasks"];
    if (!o) {
      keys.forEach((k) => {
        $("stats-kpi-" + k).textContent = "—";
        $("stats-kpi-" + k + "-sub").textContent = "—";
      });
      return;
    }
    const clusters = o.clusters || [];
    $("stats-kpi-nodes").textContent = statsNum(o.nodes);
    $("stats-kpi-nodes-sub").textContent = "последний срез " + fmtStamp(o.lastSampleAt) +
      (o.cluster ? " · кластер " + o.cluster : " · кластеров " + clusters.length);
    $("stats-kpi-images").textContent = statsNum(o.openImages);
    $("stats-kpi-images-sub").textContent = fmtBytes(Number(o.openImagesBytes) || 0) +
      " · медиана жизни " + fmtSeconds(o.medianLifetimeSeconds) +
      " · p95 " + fmtSeconds(o.p95LifetimeSeconds);
    $("stats-kpi-gc").textContent = statsNum(o.gcRuns);
    $("stats-kpi-gc-sub").textContent = "удалено " + statsNum(o.gcRemovedImages) + " образов · " +
      fmtBytes(Number(o.gcRemovedBytes) || 0);
    $("stats-kpi-repulls").textContent = statsNum(o.repulls);
    $("stats-kpi-repulls-sub").textContent = "медианный лаг " + fmtSeconds(o.repullMedianLagSeconds) +
      " · макс " + fmtSeconds(o.repullMaxLagSeconds);
    $("stats-kpi-tasks").textContent = statsNum(o.tasks);
    $("stats-kpi-tasks").style.color = (Number(o.tasksFailed) || 0) > 0 ? "var(--amber)" : "";
    $("stats-kpi-tasks-sub").textContent = statsNum(o.tasksSucceeded) + " ok · " + statsNum(o.tasksFailed) +
      " fail · " + statsNum(o.tasksActive) + " активных · ср. " + fmtDur(Number(o.tasksAvgDurationMs) || 0);
  }

  // The nodes endpoint takes the same cluster filter as the lists, so these rows
  // are already narrowed on the server and the header only says which slice is
  // on screen.
  function renderStatsNodes() {
    const st = state.stats;
    const errEl = $("stats-nodes-err");
    errEl.textContent = st.nodesError ? "Не удалось загрузить ноды: " + st.nodesError : "";
    errEl.classList.toggle("hidden", !st.nodesError);
    const rows = st.nodes || [];
    $("stats-nodes-summary").textContent = rows.length === 0
      ? (st.cluster ? "нет данных по кластеру " + st.cluster : "нет данных")
      : rows.length + " нод · " + (st.cluster ? "кластер " + st.cluster : "все кластеры");
    const tbody = $("stats-nodes-tbody");
    if (rows.length === 0) {
      tbody.innerHTML = `<tr><td colspan="8" class="empty-row">` +
        (st.cluster
          ? "По кластеру " + esc(st.cluster) + " нет последних срезов нод"
          : "Нет последних срезов: сборщик ещё не записал ни одной ноды") +
        `</td></tr>`;
      return;
    }
    tbody.innerHTML = rows.map((n) => {
      const used = Number(n.imageFsUsedBytes) || 0;
      const cap = Number(n.imageFsCapacityBytes) || 0;
      let fs;
      if (cap > 0) {
        const pct = (used / cap) * 100;
        const tone = pct >= 90 ? "stats-pct-red" : pct >= 75 ? "stats-pct-amber" : "muted";
        fs = `<span class="mono">${fmtBytes(used)} / ${n.imageFsCapacityEstimated ? "~" : ""}${fmtBytes(cap)}</span>` +
          ` <span class="${tone}">(${pct.toFixed(1)}%)</span>` +
          (n.imageFsCapacityEstimated
            ? ` <span class="pill pill-neutral stats-pill-tiny" title="CRI вернул только used: ёмкость — оценка из ephemeral-storage ноды">est.</span>`
            : "");
      } else if (used > 0) {
        fs = `<span class="mono">${fmtBytes(used)}</span> <span class="muted">/ ёмкость неизвестна</span>`;
      } else {
        fs = `<span class="muted">—</span>`;
      }
      const ready = n.ready ? `<span class="pill pill-green">Ready</span>` : `<span class="pill pill-red">NotReady</span>`;
      const agent = n.agentUp ? `<span class="pill pill-green">Agent UP</span>` : `<span class="pill pill-red">Agent DOWN</span>`;
      const disk = n.diskPaused ? `<span class="pill pill-purple">Disk Paused</span>` : `<span class="muted">—</span>`;
      const cached = n.cachedImageCount === null || n.cachedImageCount === undefined ? "—" : statsNum(n.cachedImageCount);
      return `<tr>` +
        `<td class="mono muted">${statsText(n.cluster)}</td>` +
        `<td class="mono">${statsText(n.node)}</td>` +
        `<td class="mono muted stats-nowrap">${fmtStamp(n.sampledAt)}</td>` +
        `<td>${fs}</td>` +
        `<td class="right mono">${cached}</td>` +
        `<td>${ready}</td>` +
        `<td>${agent}</td>` +
        `<td>${disk}</td>` +
        `</tr>`;
    }).join("");
  }

  function statsImageRow(r) {
    const removed = r.removedAt
      ? `<span class="mono">${fmtStamp(r.removedAt)}</span>`
      : `<span class="pill pill-green stats-pill-tiny" title="Образ всё ещё лежит на ноде">на ноде</span>`;
    const firstTitle = r.lastSeenAt ? ` title="последний раз виден: ${esc(fmtStamp(r.lastSeenAt))}"` : "";
    return `<tr>` +
      `<td class="mono muted">${statsText(r.cluster)}</td>` +
      `<td class="mono">${statsText(r.node)}</td>` +
      `<td><span class="mono stats-img" title="${esc(r.image || "")}">${statsText(r.image)}</span></td>` +
      `<td class="mono muted stats-nowrap"${firstTitle}>${fmtStamp(r.firstSeenAt)}</td>` +
      `<td class="stats-nowrap">${removed}</td>` +
      `<td class="right mono stats-nowrap">${fmtSeconds(r.lifetimeSeconds)}</td>` +
      `<td class="right mono stats-nowrap">${fmtBytes(Number(r.sizeBytes) || 0)}</td>` +
      `</tr>`;
  }

  function statsGCRow(r) {
    return `<tr>` +
      `<td class="mono muted stats-nowrap">${fmtStamp(r.occurredAt)}</td>` +
      `<td class="mono muted">${statsText(r.cluster)}</td>` +
      `<td class="mono">${statsText(r.node)}</td>` +
      `<td class="right mono">${statsNum(r.removedCount)}</td>` +
      `<td class="right mono">${fmtBytes(Number(r.removedBytes) || 0)}</td>` +
      `<td class="right mono">${fmtSeconds(r.windowSeconds)}</td>` +
      `</tr>`;
  }

  function statsRepullRow(r) {
    return `<tr>` +
      `<td class="mono muted stats-nowrap">${fmtStamp(r.removedAt)}</td>` +
      `<td class="mono muted stats-nowrap">${fmtStamp(r.finishedAt)}</td>` +
      `<td class="right mono stats-nowrap">${fmtSeconds(r.lagSeconds)}</td>` +
      `<td class="mono muted">${statsText(r.cluster)}</td>` +
      `<td class="mono">${statsText(r.node)}</td>` +
      `<td><span class="mono stats-img" title="${esc(r.image || "")}">${statsText(r.image)}</span></td>` +
      `</tr>`;
  }

  function statsTaskRow(r) {
    const lbl = taskStateLabel(r.state);
    const trig = r.trigger
      ? (r.actor ? r.trigger + " · " + r.actor : String(r.trigger))
      : (r.actor ? String(r.actor) : "—");
    const err = r.error
      ? `<span class="cell-err cell-err-clip" title="${esc(r.error)}">${esc(r.error)}</span>`
      : `<span class="muted">—</span>`;
    return `<tr>` +
      `<td class="mono muted stats-nowrap">${fmtStamp(r.observedAt)}</td>` +
      `<td class="mono muted">${statsText(r.cluster)}</td>` +
      `<td class="mono">${statsText(r.node)}</td>` +
      `<td><span class="mono stats-img" title="${esc(r.image || "")}">${statsText(r.image)}</span></td>` +
      `<td><span class="pill ${lbl.cls}">${esc(lbl.text)}</span></td>` +
      `<td>${esc(trig)}</td>` +
      `<td class="right mono stats-nowrap">${fmtDur(Number(r.durationMs) || 0)}</td>` +
      `<td>${err}</td>` +
      `</tr>`;
  }

  function renderStatsPagination(name) {
    const st = state.stats;
    const page = st.pages[name];
    const rows = st.rows[name] || [];
    const total = page.total || 0;
    const from = total === 0 ? 0 : Math.min(page.offset + 1, total);
    const to = Math.min(page.offset + rows.length, total);
    const totalPages = Math.max(Math.ceil(total / page.limit), 1);
    const current = Math.min(Math.floor(page.offset / page.limit) + 1, totalPages);
    const info = $("stats-" + name + "-page-info");
    info.textContent = total === 0 ? "0" : from + "–" + to + " из " + total + " · стр. " + current + "/" + totalPages;
    $("stats-" + name + "-prev").disabled = st.loading || page.offset <= 0;
    $("stats-" + name + "-next").disabled = st.loading || page.offset + page.limit >= total;
    const totalEl = $("stats-" + name + "-total");
    if (totalEl) totalEl.textContent = total > 0 ? "всего " + total : "";
  }

  function renderStatsList(name) {
    const st = state.stats;
    const tbody = $("stats-" + name + "-tbody");
    if (!tbody) return;
    const msg = st.errorFor[name] || "";
    const errEl = $("stats-" + name + "-err");
    errEl.textContent = msg ? "Не удалось загрузить: " + msg : "";
    errEl.classList.toggle("hidden", !msg);
    renderStatsPagination(name);
    const rows = st.rows[name] || [];
    if (rows.length === 0) {
      tbody.innerHTML = `<tr><td colspan="${STATS_COLS[name] || 6}" class="empty-row">Нет данных за выбранное окно</td></tr>`;
      return;
    }
    const renderRow = {
      images: statsImageRow,
      gc: statsGCRow,
      repulls: statsRepullRow,
      tasks: statsTaskRow,
    }[name];
    tbody.innerHTML = rows.map(renderRow).join("");
  }

  // All stats controls are static markup, so the handlers are attached once at
  // boot instead of after every render - a reload never loses input focus.
  function bindStatsControls() {
    const st = state.stats;
    $("stats-window").addEventListener("change", (e) => {
      st.window = e.target.value || "24h";
      reloadStats({ resetPages: true });
    });
    $("stats-cluster").addEventListener("change", (e) => {
      st.cluster = e.target.value || "";
      reloadStats({ resetPages: true });
    });
    $("stats-refresh").addEventListener("click", () => loadStats());
    $("stats-retry").addEventListener("click", () => loadStats());
    $("stats-retry-disabled").addEventListener("click", () => loadStats());

    STATS_LIST_NAMES.forEach((name) => {
      $("stats-" + name + "-prev").addEventListener("click", () => {
        const page = st.pages[name];
        page.offset = Math.max(page.offset - page.limit, 0);
        loadStatsList(name);
      });
      $("stats-" + name + "-next").addEventListener("click", () => {
        const page = st.pages[name];
        page.offset += page.limit;
        loadStatsList(name);
      });
      $("stats-" + name + "-page-size").addEventListener("change", (e) => {
        const page = st.pages[name];
        page.limit = Number(e.target.value) || 50;
        page.offset = 0;
        loadStatsList(name);
      });
    });

    // Typing is debounced: an image search would otherwise fire a query per key.
    let imagesTimer = null;
    $("stats-images-q").addEventListener("input", (e) => {
      st.filters.images.q = e.target.value.trim();
      clearTimeout(imagesTimer);
      imagesTimer = setTimeout(() => {
        st.pages.images.offset = 0;
        loadStatsList("images");
      }, 300);
    });
    $("stats-images-state").addEventListener("change", (e) => {
      st.filters.images.state = e.target.value || "";
      st.pages.images.offset = 0;
      loadStatsList("images");
    });

    let tasksTimer = null;
    $("stats-tasks-q").addEventListener("input", (e) => {
      st.filters.tasks.q = e.target.value.trim();
      clearTimeout(tasksTimer);
      tasksTimer = setTimeout(() => {
        st.pages.tasks.offset = 0;
        loadStatsList("tasks");
      }, 300);
    });
    $("stats-tasks-state").addEventListener("change", (e) => {
      st.filters.tasks.state = e.target.value || "";
      st.pages.tasks.offset = 0;
      loadStatsList("tasks");
    });
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
    state.nodeImageQuery = "";
    state.tasks = [];
    state.taskPage.offset = 0;
    closeNodeDetail();
    refresh();
    refreshTaskPage({ silent: true });
  });

  // --- Pull tasks pagination --------------------------------------------
  function resetTaskPage() {
    state.taskPage.offset = 0;
    refreshTaskPage();
  }

  $("tasks-prev").addEventListener("click", () => {
    state.taskPage.offset = Math.max(state.taskPage.offset - state.taskPage.limit, 0);
    refreshTaskPage();
  });

  $("tasks-next").addEventListener("click", () => {
    state.taskPage.offset += state.taskPage.limit;
    refreshTaskPage();
  });

  $("tasks-page-size").addEventListener("change", (e) => {
    state.taskPage.limit = Number(e.target.value) || 50;
    resetTaskPage();
  });

  $("tasks-state").addEventListener("change", (e) => {
    state.taskPage.state = e.target.value || "all";
    resetTaskPage();
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
    if (state.autoRefresh) {
      startPolling();
      if (state.activeTab === "stats") startStatsTimer();
    } else {
      stopPolling();
    }
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && state.nodeDetail) closeNodeDetail();
  });

  document.querySelectorAll(".tab").forEach((btn) => {
    btn.addEventListener("click", () => {
      state.activeTab = btn.dataset.tab;
      document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t === btn));
      document.querySelectorAll(".tab-panel").forEach((p) => p.classList.toggle("hidden", p.id !== `tab-${state.activeTab}`));
      // The tasks tab has its own paged feed; refresh it on activation so the
      // operator does not look at a page that the 5 s snapshot poll did not touch.
      if (state.activeTab === "tasks") refreshTaskPage({ silent: true });
      // Stats is a slow feed with its own timer: start it on activation and stop
      // it on leave, so a background tab never polls PostgreSQL.
      if (state.activeTab === "stats") activateStatsTab();
      else stopStatsTimer();
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

  bindStatsControls();

  // Boot
  api("/api/v1/auth/me")
    .then((data) => showApp(data.username, data.role))
    .catch(() => showLogin());
})();
