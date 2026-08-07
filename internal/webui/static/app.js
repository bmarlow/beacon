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
    const nameEl = el("span", "name" + (opts.mono ? " pill-ip" : ""), opts.name);
    row.appendChild(nameEl);
    if (opts.ns) row.appendChild(el("span", "ns", opts.ns));
    if (opts.meta) row.appendChild(el("span", "meta", opts.meta));
    row.appendChild(el("span", "spacer"));
    if (opts.adv) row.appendChild(el("span", "adv", opts.adv));
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
    if (!p.probed) meta.push("no probes");
    else meta.push(p.ready ? "ready" : "not ready");
    return nodeRow({
      id: "pod/" + p.namespace + "/" + p.name,
      kind: "Pod",
      name: p.name,
      ns: p.namespace,
      meta: meta.join(" \u00b7 "),
      status: p.status,
    });
  }

  function serviceNode(s) {
    const kids = (s.pods || []).map(podNode);
    return nodeRow({
      id: "svc/" + s.namespace + "/" + s.name,
      kind: "Service",
      name: s.name,
      ns: s.namespace,
      meta: (s.type || "") + (s.pods ? " \u00b7 " + s.pods.length + " pod(s)" : ""),
      status: s.status,
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
      children: kids,
    });
  }

  function gatewayNode(g) {
    const kids = (g.routes || []).map(routeNode);
    const meta = [];
    if (g.className) meta.push("class=" + g.className);
    if (g.exempt) meta.push("exempt");
    if (g.ips && g.ips.length) meta.push(g.ips.join(", "));
    return nodeRow({
      id: "gw/" + g.namespace + "/" + g.name,
      kind: "Gateway",
      name: g.name,
      ns: g.namespace,
      meta: meta.join(" \u00b7 "),
      adv: g.advertisement,
      status: g.status,
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
      status: ip.status,
      children: kids,
    });
  }

  function poolNode(p) {
    const kids = (p.ips || []).map(ipNode);
    return nodeRow({
      id: "pool/" + p.name,
      kind: "IPAddressPool",
      name: p.name,
      ns: p.namespace,
      meta: (p.addresses || []).join(", "),
      status: p.status,
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
