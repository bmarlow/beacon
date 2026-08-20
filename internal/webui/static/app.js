/* Beacon topology dashboard */
(function () {
  "use strict";

  const treeEl = document.getElementById("tree");
  const summaryEl = document.getElementById("summary");
  const updatedEl = document.getElementById("updated");
  const errEl = document.getElementById("err");
  const autoEl = document.getElementById("autoRefresh");
  const legendEl = document.getElementById("legend");
  const filterInfoEl = document.getElementById("filterInfo");
  const clearFilterBtn = document.getElementById("clearFilterBtn");
  const statusChips = Array.prototype.slice.call(
    document.querySelectorAll(".chip[data-status]")
  );

  let collapsed = {}; // id -> true when collapsed (persisted across refreshes)
  let timer = null;
  let consoleBase = ""; // OpenShift console base URL from the graph

  // statusFilter holds the set of Status values currently selected in the
  // legend. Empty means "no filter" (show everything). Clicking a status
  // chip toggles membership, so multiple statuses can be active at once
  // (e.g. Unhealthy + Degraded). Persists across auto-refreshes, same as
  // `collapsed`.
  let statusFilter = new Set();

  // statusVisible reports whether a node with the given status should be
  // shown given the current filter (no filter => everything visible).
  function statusVisible(status) {
    return statusFilter.size === 0 || statusFilter.has(status || "Unknown");
  }

  // consoleURL builds an OpenShift console URL for a resource ref, or "" if a
  // link can't be built (no console base, no ref).
  //
  // Console paths:
  //   namespaced core:   /k8s/ns/<ns>/<plural>/<name>
  //   namespaced CRD:    /k8s/ns/<ns>/<group>~<version>~<Kind>/<name>
  //   cluster core:      /k8s/cluster/<plural>/<name>
  //   cluster CRD:       /k8s/cluster/<group>~<version>~<Kind>/<name>
  function consoleURL(ref) {
    if (!consoleBase || !ref || !ref.name) return "";
    let resource;
    if (ref.group) {
      resource = ref.group + "~" + (ref.version || "v1") + "~" + ref.kind;
    } else {
      resource = ref.plural || (ref.kind || "").toLowerCase() + "s";
    }
    const scope = ref.clusterScoped || !ref.namespace
      ? "cluster"
      : "ns/" + encodeURIComponent(ref.namespace);
    return (
      consoleBase +
      "/k8s/" +
      scope +
      "/" +
      resource +
      "/" +
      encodeURIComponent(ref.name)
    );
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function badge(status) {
    const b = el("span", "badge s-" + (status || "Unknown"), status || "Unknown");
    return b;
  }

  // dur formats a number of seconds as a compact human duration, e.g.
  // 45 -> "45s", 130 -> "2m10s", 3720 -> "1h2m", 90000 -> "1d1h".
  function dur(s) {
    s = Math.max(0, Math.floor(s));
    const d = Math.floor(s / 86400);
    const h = Math.floor((s % 86400) / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (d > 0) return d + "d" + (h ? h + "h" : "");
    if (h > 0) return h + "h" + (m ? m + "m" : "");
    if (m > 0) return m + "m" + (sec ? sec + "s" : "");
    return sec + "s";
  }

  // timerChip renders a running dampening timer, e.g.:
  //   "backoff 2s / 5s (3s left)"  or  "recovery 12s / 30s (18s left)"
  function timerChip(t) {
    const label =
      t.kind +
      " " +
      t.elapsedSeconds +
      "s / " +
      t.thresholdSeconds +
      "s (" +
      t.remainingSeconds +
      "s left)";
    const c = el("span", "timer timer-" + t.kind, label);
    c.title =
      t.kind === "backoff"
        ? "Backends unhealthy for " +
          t.elapsedSeconds +
          "s; Gateway proxy scales to 0 (withdraw) after " +
          t.thresholdSeconds +
          "s."
        : "Backends healthy for " +
          t.elapsedSeconds +
          "s; Gateway proxy scales back up (re-advertise) after " +
          t.thresholdSeconds +
          "s.";
    return c;
  }

  function toggleFor(id, hasChildren) {
    const t = el("span", "toggle" + (hasChildren ? "" : " empty"),
      collapsed[id] ? "+" : "\u2212");
    if (hasChildren) {
      t.addEventListener("click", function () {
        collapsed[id] = !collapsed[id];
        render(lastGraph);
      });
    }
    return t;
  }

  function nodeRow(opts) {
    // opts: {id, kind, name, ns, meta, adv, status, children[]}
    const wrap = el("div", "node st-" + (opts.status || "Unknown"));
    const row = el("div", "row");
    const hasChildren = opts.children && opts.children.length > 0;

    row.appendChild(toggleFor(opts.id, hasChildren));
    if (opts.kind) row.appendChild(el("span", "kind", opts.kind));
    // Name: a console link when we have a base URL + ref, else plain text.
    const href = consoleURL(opts.ref);
    let nameEl;
    if (href) {
      nameEl = el("a", "name link" + (opts.mono ? " pill-ip" : ""), opts.name);
      nameEl.href = href;
      nameEl.target = "_blank";
      nameEl.rel = "noopener noreferrer";
      nameEl.title = "Open in OpenShift console";
    } else {
      nameEl = el("span", "name" + (opts.mono ? " pill-ip" : ""), opts.name);
    }
    row.appendChild(nameEl);
    // Optional inline tags (e.g. a "CRITICAL" chip) rendered right after the name.
    if (opts.tags && opts.tags.length) {
      opts.tags.forEach((tg) => {
        const tagEl = el("span", "tag " + (tg.cls || ""), tg.label);
        if (tg.title) tagEl.title = tg.title;
        row.appendChild(tagEl);
      });
    }
    if (opts.ns) row.appendChild(el("span", "ns", opts.ns));
    if (opts.meta) {
      const metaEl = el("span", "meta", opts.meta);
      if (opts.metaTitle) metaEl.title = opts.metaTitle;
      row.appendChild(metaEl);
    }
    row.appendChild(el("span", "spacer"));
    if (opts.timer) row.appendChild(timerChip(opts.timer));
    if (opts.adv) row.appendChild(el("span", "adv", opts.adv));
    if (opts.statusForSeconds && opts.statusForSeconds > 0) {
      const since = el("span", "since", "for " + dur(opts.statusForSeconds));
      since.title = "In status \u201c" + (opts.status || "Unknown") + "\u201d for " + dur(opts.statusForSeconds);
      row.appendChild(since);
    }
    row.appendChild(badge(opts.status));
    wrap.appendChild(row);

    if (hasChildren && !collapsed[opts.id]) {
      const kids = el("div", "children");
      opts.children.forEach((c) => kids.appendChild(c));
      wrap.appendChild(kids);
    }
    return wrap;
  }

  function podNode(p) {
    if (!statusVisible(p.status)) return null;
    const meta = [p.phase];
    if (p.node) meta.push("@" + p.node);
    if (p.remote) {
      meta.push(p.ready ? "link ready" : "link down");
      if (p.reason) meta.push(p.reason);
    } else if (!p.probed) {
      meta.push("no probes");
    } else {
      meta.push(p.ready ? "ready" : "not ready");
    }
    return nodeRow({
      id: "pod/" + p.namespace + "/" + p.name,
      kind: p.remote ? "Remote" : "Pod",
      name: p.name,
      ns: p.remote ? "" : p.namespace,
      meta: meta.join(" \u00b7 "),
      status: p.status,
      statusForSeconds: p.statusForSeconds,
      ref: p.ref,
    });
  }

  function serviceNode(s) {
    const kids = (s.pods || []).map(podNode).filter(Boolean);
    // Hide this Service entirely when a filter is active, none of its pods
    // matched, and the Service's own status doesn't match either.
    if (!statusVisible(s.status) && kids.length === 0) return null;
    let meta;
    if (s.skupper) {
      meta =
        "\u{1F517} Skupper link \u00b7 listener=" +
        s.skupper.listenerName +
        " \u00b7 " +
        (s.skupper.ready ? "remote ready" : "remote unavailable");
    } else if (s.scaledToZero) {
      // Selector but zero pods, treated as unhealthy by the zeroReplicasPolicy.
      meta = (s.type || "") + " \u00b7 \u26A0 scaled to zero";
    } else {
      // Show the ready/probed pod ratio (probe-less pods are not counted).
      const probed = (s.pods || []).filter((p) => p.probed);
      const ready = probed.filter((p) => p.ready).length;
      let podInfo = "";
      if (probed.length > 0) {
        podInfo = " \u00b7 pods " + ready + "/" + probed.length + " ready";
      } else if (s.pods && s.pods.length) {
        podInfo = " \u00b7 " + s.pods.length + " pod(s), no probes";
      }
      meta = (s.type || "") + podInfo;
    }
    const tags = [];
    if (s.critical) {
      tags.push({
        label: "CRITICAL",
        cls: "tag-critical",
        title:
          "Critical backend: if this Service becomes unavailable, the whole " +
          "Gateway is withdrawn regardless of the min-healthy-backend threshold.",
      });
    }
    return nodeRow({
      id: "svc/" + s.namespace + "/" + s.name,
      kind: s.skupper ? "Service (Skupper)" : "Service",
      name: s.name,
      ns: s.namespace,
      meta: meta,
      tags: tags,
      status: s.status,
      statusForSeconds: s.statusForSeconds,
      ref: s.ref,
      children: kids,
    });
  }

  function routeNode(r) {
    const kids = (r.services || []).map(serviceNode).filter(Boolean);
    if (!statusVisible(r.status) && kids.length === 0) return null;
    let meta = "";
    if (r.hostnames && r.hostnames.length) meta = r.hostnames.join(", ");
    return nodeRow({
      id: "route/" + r.namespace + "/" + r.kind + "/" + r.name,
      kind: r.kind,
      name: r.name,
      ns: r.namespace,
      meta: meta,
      status: r.status,
      statusForSeconds: r.statusForSeconds,
      ref: r.ref,
      children: kids,
    });
  }

  function gatewayNode(g) {
    const kids = (g.routes || []).map(routeNode).filter(Boolean);
    if (!statusVisible(g.status) && kids.length === 0) return null;
    const meta = [];
    if (g.className) meta.push("class=" + g.className);
    if (g.exempt) meta.push("exempt");
    // Proxy replica count (data-plane).
    meta.push("replicas " + (g.replicasReady || 0) + "/" + (g.replicasDesired || 0));
    // Backend health ratio vs. the min-healthy threshold.
    if (g.countedBackends && g.countedBackends > 0) {
      meta.push(
        "backends " + (g.healthyBackends || 0) + "/" + g.countedBackends +
        " (min " + (g.minHealthyPercent != null ? g.minHealthyPercent : 100) + "%)"
      );
    }
    if (g.criticalBackendDown) meta.push("\u26A0 critical backend down");
    if (g.ips && g.ips.length) meta.push(g.ips.join(", "));
    const tags = [];
    if (g.criticalBackendDown) {
      tags.push({
        label: "CRITICAL DOWN",
        cls: "tag-critical",
        title:
          "A backend flagged critical is unavailable; the Gateway is withdrawn " +
          "regardless of the min-healthy-backend threshold.",
      });
    }
    return nodeRow({
      id: "gw/" + g.namespace + "/" + g.name,
      kind: "Gateway",
      name: g.name,
      ns: g.namespace,
      meta: meta.join(" \u00b7 "),
      tags: tags,
      adv: g.advertisement,
      timer: g.timer,
      status: g.status,
      statusForSeconds: g.statusForSeconds,
      ref: g.ref,
      children: kids,
    });
  }

  function ipNode(ip) {
    const kids = (ip.gateways || []).map(gatewayNode).filter(Boolean);
    if (!statusVisible(ip.status) && kids.length === 0) return null;
    return nodeRow({
      id: "ip/" + ip.ip,
      kind: "VIP",
      name: ip.ip,
      mono: true,
      adv: ip.advertisement,
      timer: ip.timer,
      statusForSeconds: ip.statusForSeconds,
      status: ip.status,
      children: kids,
    });
  }

  function poolNode(p) {
    const kids = (p.ips || []).map(ipNode).filter(Boolean);
    if (!statusVisible(p.status) && kids.length === 0) return null;
    const meta = [(p.addresses || []).join(", ")];
    if (p.restricted) meta.push("\u{1F512} restricted");
    return nodeRow({
      id: "pool/" + p.name,
      kind: "IPAddressPool",
      name: p.name,
      ns: p.namespace,
      meta: meta.filter(Boolean).join(" \u00b7 "),
      metaTitle: p.restricted
        ? "You do not have read access to this MetalLB pool; it is shown for context because it backs a Gateway you can see."
        : "",
      status: p.status,
      statusForSeconds: p.statusForSeconds,
      ref: p.ref,
      children: kids,
    });
  }

  function stat(label, value, warn) {
    const s = el("div", "stat" + (warn ? " warn" : ""));
    s.appendChild(el("b", null, String(value)));
    s.appendChild(el("span", null, label));
    return s;
  }

  function renderSummary(sum) {
    summaryEl.innerHTML = "";
    summaryEl.appendChild(stat("Pools", sum.pools));
    summaryEl.appendChild(stat("Gateways", sum.gateways));
    summaryEl.appendChild(stat("Routes", sum.routes));
    summaryEl.appendChild(stat("Services", sum.services));
    summaryEl.appendChild(stat("Pods", sum.pods));
    summaryEl.appendChild(stat("Advertised", sum.advertisedIPs));
    summaryEl.appendChild(stat("Withdrawn", sum.withdrawnIPs, sum.withdrawnIPs > 0));
    summaryEl.appendChild(stat("Unhealthy GW", sum.unhealthyGateways, sum.unhealthyGateways > 0));
  }

  // countStatuses walks the *unfiltered* graph and tallies how many nodes
  // (of any kind - pools, IPs, gateways, routes, services, pods) currently
  // have each status, so the legend chips can show live counts regardless
  // of the active filter.
  function countStatuses(graph) {
    const counts = {};
    function bump(status) {
      const k = status || "Unknown";
      counts[k] = (counts[k] || 0) + 1;
    }
    function walkPod(p) { bump(p.status); }
    function walkService(s) { bump(s.status); (s.pods || []).forEach(walkPod); }
    function walkRoute(r) { bump(r.status); (r.services || []).forEach(walkService); }
    function walkGateway(g) { bump(g.status); (g.routes || []).forEach(walkRoute); }
    function walkIP(ip) { bump(ip.status); (ip.gateways || []).forEach(walkGateway); }
    function walkPool(p) { bump(p.status); (p.ips || []).forEach(walkIP); }
    (graph.pools || []).forEach(walkPool);
    (graph.unpooledGateways || []).forEach(walkGateway);
    return counts;
  }

  // updateLegendUI reflects the current statusFilter onto the legend chips
  // (active/inactive styling, aria-pressed, live counts) and the "Clear
  // filter" control.
  function updateLegendUI() {
    const counts = lastGraph ? countStatuses(lastGraph) : {};
    statusChips.forEach((chip) => {
      const active = statusFilter.has(chip.dataset.status);
      chip.classList.toggle("active", active);
      chip.setAttribute("aria-pressed", active ? "true" : "false");
      chip.textContent =
        chip.dataset.label + " (" + (counts[chip.dataset.status] || 0) + ")";
    });
    legendEl.classList.toggle("filtering", statusFilter.size > 0);
    if (statusFilter.size > 0) {
      clearFilterBtn.style.display = "";
      filterInfoEl.textContent =
        "Showing only: " + Array.from(statusFilter).join(", ");
    } else {
      clearFilterBtn.style.display = "none";
      filterInfoEl.textContent = "";
    }
  }

  function toggleStatusFilter(status) {
    if (statusFilter.has(status)) {
      statusFilter.delete(status);
    } else {
      statusFilter.add(status);
    }
    render(lastGraph);
  }

  function clearStatusFilter() {
    if (statusFilter.size === 0) return;
    statusFilter.clear();
    render(lastGraph);
  }

  statusChips.forEach((chip) => {
    chip.addEventListener("click", () => toggleStatusFilter(chip.dataset.status));
  });
  clearFilterBtn.addEventListener("click", clearStatusFilter);

  let lastGraph = null;

  function render(graph) {
    lastGraph = graph;
    if (!graph) return;
    consoleBase = (graph.consoleBaseURL || "").replace(/\/$/, "");
    const verEl = document.getElementById("version");
    if (verEl) verEl.textContent = graph.operatorVersion ? "v" + graph.operatorVersion.replace(/^v/, "") : "";
    // Show the authenticated user and the logout button only when auth is on.
    const userEl = document.getElementById("user");
    const logoutEl = document.getElementById("logoutBtn");
    if (graph.user) {
      if (userEl) userEl.textContent = graph.user;
      if (logoutEl) logoutEl.style.display = "";
    } else {
      if (userEl) userEl.textContent = "";
      if (logoutEl) logoutEl.style.display = "none";
    }
    renderSummary(graph.summary || {});
    updateLegendUI();
    treeEl.innerHTML = "";

    const rawPools = graph.pools || [];
    const rawUnpooled = graph.unpooledGateways || [];
    const poolNodes = rawPools.map(poolNode).filter(Boolean);
    const unpooledNodes = rawUnpooled.map(gatewayNode).filter(Boolean);

    if (rawPools.length === 0 && rawUnpooled.length === 0) {
      treeEl.appendChild(el("div", "loading", "No MetalLB pools or gateways found."));
    } else if (poolNodes.length === 0 && unpooledNodes.length === 0) {
      treeEl.appendChild(
        el("div", "loading", "No items match the selected status filter.")
      );
    }

    poolNodes.forEach((n) => treeEl.appendChild(n));

    if (unpooledNodes.length) {
      const wrap = nodeRow({
        id: "group/unpooled",
        kind: "Group",
        name: "Gateways not sourced from MetalLB",
        status: "Unknown",
        children: unpooledNodes,
      });
      treeEl.appendChild(wrap);
    }
  }

  function setAllCollapsed(val) {
    if (!lastGraph) return;
    const walk = (id, children) => {
      if (children && children.length) collapsed[id] = val;
    };
    (lastGraph.pools || []).forEach((p) => {
      walk("pool/" + p.name, p.ips);
      (p.ips || []).forEach((ip) => {
        walk("ip/" + ip.ip, ip.gateways);
        (ip.gateways || []).forEach((g) => {
          walk("gw/" + g.namespace + "/" + g.name, g.routes);
          (g.routes || []).forEach((r) => {
            walk("route/" + r.namespace + "/" + r.kind + "/" + r.name, r.services);
            (r.services || []).forEach((s) => walk("svc/" + s.namespace + "/" + s.name, s.pods));
          });
        });
      });
    });
    render(lastGraph);
  }

  // refreshInFlight prevents overlapping requests. Without this, if a single
  // refresh takes longer than the auto-refresh interval (e.g. a slow/cold
  // cluster), setInterval would keep firing new fetches on top of the one
  // still in progress; each overlapping request triggers its own full
  // server-side rebuild (its own burst of concurrent API calls), which
  // competes with the others for the same rate-limited client and gets
  // slower, causing still more overlap on the next tick — a thundering-herd
  // pileup that compounds without bound. Guarding re-entrancy here (and
  // driving auto-refresh with a self-rescheduling setTimeout below, instead
  // of a fixed setInterval) guarantees at most one in-flight request from
  // this tab at a time.
  let refreshInFlight = false;

  async function refresh() {
    if (refreshInFlight) return;
    refreshInFlight = true;
    try {
      const res = await fetch("api/topology", { headers: { Accept: "application/json" } });
      if (!res.ok) throw new Error("HTTP " + res.status + ": " + (await res.text()));
      const graph = await res.json();
      errEl.textContent = "";
      render(graph);
      updatedEl.textContent = "Updated " + new Date().toLocaleTimeString();
    } catch (e) {
      errEl.textContent = "Error: " + e.message;
    } finally {
      refreshInFlight = false;
    }
  }

  // scheduleAuto drives auto-refresh with a self-rescheduling setTimeout
  // rather than setInterval: the next refresh is only queued 5s AFTER the
  // current one finishes (success or failure), never while one is still in
  // flight. This naturally paces requests to the server's actual
  // responsiveness instead of hammering it on a fixed clock.
  function scheduleAuto() {
    if (timer) clearTimeout(timer);
    if (!autoEl.checked) return;
    timer = setTimeout(async () => {
      await refresh();
      scheduleAuto();
    }, 5000);
  }

  document.getElementById("refreshBtn").addEventListener("click", refresh);
  document.getElementById("expandBtn").addEventListener("click", () => setAllCollapsed(false));
  document.getElementById("collapseBtn").addEventListener("click", () => setAllCollapsed(true));
  autoEl.addEventListener("change", scheduleAuto);

  refresh();
  scheduleAuto();
})();
