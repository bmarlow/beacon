/* Beacon topology dashboard */
(function () {
  "use strict";

  const treeEl = document.getElementById("tree");
  const summaryEl = document.getElementById("summary");
  const updatedEl = document.getElementById("updated");
  const errEl = document.getElementById("err");
  const autoEl = document.getElementById("autoRefresh");

  let collapsed = {}; // id -> true when collapsed (persisted across refreshes)
  let timer = null;
  let consoleBase = ""; // OpenShift console base URL from the graph

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
    const kids = (s.pods || []).map(podNode);
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
    return nodeRow({
      id: "svc/" + s.namespace + "/" + s.name,
      kind: s.skupper ? "Service (Skupper)" : "Service",
      name: s.name,
      ns: s.namespace,
      meta: meta,
      status: s.status,
      statusForSeconds: s.statusForSeconds,
      ref: s.ref,
      children: kids,
    });
  }

  function routeNode(r) {
    const kids = (r.services || []).map(serviceNode);
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
    const kids = (g.routes || []).map(routeNode);
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
    if (g.ips && g.ips.length) meta.push(g.ips.join(", "));
    return nodeRow({
      id: "gw/" + g.namespace + "/" + g.name,
      kind: "Gateway",
      name: g.name,
      ns: g.namespace,
      meta: meta.join(" \u00b7 "),
      adv: g.advertisement,
      timer: g.timer,
      status: g.status,
      statusForSeconds: g.statusForSeconds,
      ref: g.ref,
      children: kids,
    });
  }

  function ipNode(ip) {
    const kids = (ip.gateways || []).map(gatewayNode);
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
    const kids = (p.ips || []).map(ipNode);
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
    treeEl.innerHTML = "";

    const pools = graph.pools || [];
    if (pools.length === 0 && (!graph.unpooledGateways || graph.unpooledGateways.length === 0)) {
      treeEl.appendChild(el("div", "loading", "No MetalLB pools or gateways found."));
    }
    pools.forEach((p) => treeEl.appendChild(poolNode(p)));

    if (graph.unpooledGateways && graph.unpooledGateways.length) {
      const wrap = nodeRow({
        id: "group/unpooled",
        kind: "Group",
        name: "Gateways not sourced from MetalLB",
        status: "Unknown",
        children: graph.unpooledGateways.map(gatewayNode),
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

  async function refresh() {
    try {
      const res = await fetch("api/topology", { headers: { Accept: "application/json" } });
      if (!res.ok) throw new Error("HTTP " + res.status + ": " + (await res.text()));
      const graph = await res.json();
      errEl.textContent = "";
      render(graph);
      updatedEl.textContent = "Updated " + new Date().toLocaleTimeString();
    } catch (e) {
      errEl.textContent = "Error: " + e.message;
    }
  }

  function scheduleAuto() {
    if (timer) clearInterval(timer);
    if (autoEl.checked) timer = setInterval(refresh, 5000);
  }

  document.getElementById("refreshBtn").addEventListener("click", refresh);
  document.getElementById("expandBtn").addEventListener("click", () => setAllCollapsed(false));
  document.getElementById("collapseBtn").addEventListener("click", () => setAllCollapsed(true));
  autoEl.addEventListener("change", scheduleAuto);

  refresh();
  scheduleAuto();
})();
