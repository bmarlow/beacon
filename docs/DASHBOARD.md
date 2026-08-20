# Beacon dashboard architecture

This document is a deep dive into **how the topology dashboard (web UI) is
built, served, rendered, cached, and authenticated** — the companion to
[`HEALTH.md`](HEALTH.md), which covers *what* the dashboard shows (status
semantics); this document covers *how* it shows it.

> Scope: `internal/webui/` (server, auth, resource provisioning, static
> assets), `internal/topology/` (graph construction), `internal/state/`
> (cross-request live state), and `internal/rcache/` (the short-TTL cache that
> makes the whole thing fast). Not covered: the separate machine-oriented
> `/api/v1/export/summary` endpoint (`internal/export/`) — that's a
> multi-cluster hub integration, not the human dashboard.

---

## Table of contents

- [Overview](#overview)
- [Deployment topology](#deployment-topology)
- [Two operating modes: authenticated vs. unauthenticated](#two-operating-modes-authenticated-vs-unauthenticated)
- [Request lifecycle, end to end](#request-lifecycle-end-to-end)
- [The HTTP server](#the-http-server)
- [Authentication & authorization](#authentication--authorization)
- [Building the topology graph](#building-the-topology-graph)
- [The data model](#the-data-model)
- [The client: a static, dependency-free SPA](#the-client-a-static-dependency-free-spa)
- [Caching architecture](#caching-architecture)
- [Concurrency & rate limiting](#concurrency--rate-limiting)
- [Multi-replica consistency](#multi-replica-consistency)
- [Why it's built this way: a brief performance history](#why-its-built-this-way-a-brief-performance-history)
- [Security considerations](#security-considerations)
- [File map](#file-map)

---

## Overview

The dashboard is a **read-only, server-rendered-data / client-rendered-DOM**
web application with exactly one JSON API endpoint. There is no database, no
server-side HTML templating, and no client-side build step:

```
┌─────────────────────────────────────────────────────────────────────────┐
│  beacon-controller-manager pod                                          │
│                                                                          │
│   internal/webui.Server  (net/http, embedded static assets)             │
│     GET /              → index.html + app.js + style.css (go:embed)     │
│     GET /api/topology   → JSON snapshot of cluster state, built fresh   │
│     GET /healthz        → liveness                                      │
│                                                                          │
│   Every "refresh" the browser does re-fetches /api/topology and the     │
│   client (app.js) rebuilds the entire DOM tree from that JSON — there   │
│   is no incremental/diffed rendering, no websockets, no SSE.            │
└─────────────────────────────────────────────────────────────────────────┘
```

Everything the dashboard displays — Pools, VIPs, Gateways, Routes, Services,
Pods, and their statuses — is recomputed **from live cluster state** on every
single request. Nothing is pre-rendered or persisted; the "database" is the
Kubernetes API server itself (plus one piece of shared, leader-written state —
see [Multi-replica consistency](#multi-replica-consistency)).

---

## Deployment topology

The dashboard runs **inside the operator's own Deployment**, as one of two
containers per pod, fronted by OpenShift's OAuth reverse proxy when
authentication is enabled (the default for the OLM-installed operator):

```
                              OpenShift cluster
┌───────────────────────────────────────────────────────────────────────────┐
│                                                                           │
│   Browser                                                                │
│     │  1. GET https://beacon-dashboard-<ns>.apps.<cluster>/              │
│     │     (redirected to OpenShift login on first visit, see below)     │
│     ▼                                                                    │
│   Route "beacon-dashboard"  (spec.tls.termination: reencrypt)            │
│     │                                                                    │
│     ▼                                                                    │
│   Service "beacon-dashboard"  :9443 → pod port "dashboard"               │
│     │                                                                    │
│     ▼                                                                    │
│  ┌─────────────────── Pod: beacon-controller-manager-xxxxx ───────────┐ │
│  │                                                                     │ │
│  │   ┌────────────────────────┐        loopback only,        ┌──────┐│ │
│  │   │ oauth-proxy (sidecar)  │  http://127.0.0.1:8082/  ───▶│manager││ │
│  │   │ :9443 HTTPS            │◀────────────── response ──────│(webui)││ │
│  │   │ registry.redhat.io/    │                                └──────┘│ │
│  │   │ openshift4/ose-oauth-  │                                        │ │
│  │   │ proxy:v4.14            │                                        │ │
│  │   └────────────────────────┘                                       │ │
│  │             ▲                                                       │ │
│  └─────────────┼───────────────────────────────────────────────────────┘ │
│                │ OAuth login / token exchange                            │
│                ▼                                                         │
│      OpenShift OAuth server + kube-apiserver (SubjectAccessReview,      │
│                                                TokenReview)              │
└───────────────────────────────────────────────────────────────────────────┘
```

Both containers, one Service, one Route — provisioned and owned by the
operator itself at startup (`internal/webui/dashboard_resources.go`'s
`ResourceManager`, leader-elected so only one replica reconciles them):

| Resource | Owned by | Purpose |
| --- | --- | --- |
| `Service/beacon-dashboard` | operator Deployment (ownerRef) | Routes to the `dashboard`-or-`9443` container port depending on auth mode |
| `Route/beacon-dashboard` (OpenShift only) | not owner-refed (admission blocks cross-group ownerRef); reconciled idempotently instead | External HTTPS entry point |
| `ConsoleLink/beacon-dashboard` (OpenShift only) | cluster-scoped | Adds the dashboard to the console's Application Launcher, under "Observability" |
| `Secret/beacon-dashboard-tls` | service-ca operator (via annotation) | Service-serving cert for the oauth-proxy's upstream TLS |
| `Secret/beacon-dashboard-proxy` | operator | oauth-proxy's session-cookie signing secret (random, generated once, never rotated automatically) |
| `ServiceAccount/beacon-controller-manager` annotation | operator | Registers the SA as an OAuth client with a redirect reference to the Route, so `oauth-proxy --provider=openshift` can complete logins |

The dashboard is served on **every replica** (`NeedLeaderElection() bool {
return false }` on `webui.Server`) — it's read-only, so there's no correctness
reason to restrict it to the leader. See
[Multi-replica consistency](#multi-replica-consistency) for how two replicas
serving the same URL stay in agreement.

---

## Two operating modes: authenticated vs. unauthenticated

Controlled by `--dashboard-auth-required` (default `true`):

| | `--dashboard-auth-required=true` (OLM/CSV install — the default) | `--dashboard-auth-required=false` (`make deploy` / local dev) |
| --- | --- | --- |
| Fronting proxy | oauth-proxy sidecar, `:9443` HTTPS | none — manager serves plaintext directly |
| `--dashboard-bind-address` | `127.0.0.1:8082` (loopback only — unreachable except through the sidecar) | `:8082` (reachable directly) |
| Identity | Derived from oauth-proxy's forwarded headers | none (`graph.User` empty) |
| Per-object filtering | Yes — every node is checked with a `SubjectAccessReview` | No — everything is visible to anyone who can reach the port |
| Route TLS | `reencrypt` (oauth-proxy terminates with a service-serving cert) | `edge` (terminated at the router) |
| Intended use | Production / any real cluster | Local development only — **do not expose this externally** |

Both modes share the exact same `topology.Builder` and static assets; only
`webui.Server.RequireAuth` and the presence of an `Authorizer` differ.

---

## Request lifecycle, end to end

A single `GET /api/topology` request, in the authenticated (default) case:

```
Browser          oauth-proxy         webui.Server          topology.Builder      rcache/authzCache    kube-apiserver
  │                    │                    │                     │                     │                   │
  │ GET /api/topology  │                    │                     │                     │                   │
  │───────────────────▶│                    │                     │                     │                   │
  │                    │ validate session   │                     │                     │                   │
  │                    │ cookie; inject     │                     │                     │                   │
  │                    │ X-Forwarded-User,  │                     │                     │                   │
  │                    │ -Groups headers    │                     │                     │                   │
  │                    │───────────────────▶│                     │                     │                   │
  │                    │                    │ userFromRequest(r)  │                     │                   │
  │                    │                    │ NewAccessChecker    │                     │                   │
  │                    │                    │  (uses s.authzCache)│                     │                   │
  │                    │                    │ acquire buildSem    │                     │                   │
  │                    │                    │  (max 2 concurrent) │                     │                   │
  │                    │                    │────────────────────▶│                     │                   │
  │                    │                    │                     │ list Gateways,      │                   │
  │                    │                    │                     │ Routes, Policy,     │                   │
  │                    │                    │                     │ IPAddressPools      │──────────────────▶│
  │                    │                    │                     │◀────────────────────┼───────────────────│
  │                    │                    │                     │ fan out: up to 20   │                   │
  │                    │                    │                     │ Gateways in parallel│                   │
  │                    │                    │                     │  ├─ canRead(gw)? ──▶│  cache? ──miss───▶│ SAR
  │                    │                    │                     │  ├─ getService() ──▶│  cache? ──miss───▶│ Get
  │                    │                    │                     │  ├─ podsForService─▶│  cache? ──miss───▶│ List/Get
  │                    │                    │                     │  └─ proxyReplicas ─▶│  cache? ──miss───▶│ List
  │                    │                    │                     │◀────────────────────┼───────────────────│
  │                    │                    │ release buildSem    │  assemble Graph      │                   │
  │                    │                    │◀────────────────────│                     │                   │
  │                    │◀───────────────────│ JSON-encode & write │                     │                   │
  │◀───────────────────│                    │                     │                     │                   │
  │  render(graph) in app.js: rebuild the DOM tree                                                           │
```

On a cache-warm poll (the common case — see
[Caching architecture](#caching-architecture)), almost every arrow into
"kube-apiserver" is skipped entirely; the whole round trip typically completes
in well under a second.

---

## The HTTP server

`internal/webui/server.go`'s `Server` implements `manager.Runnable` so the
controller-runtime `Manager` owns its lifecycle (started/stopped alongside the
reconciler, graceful 5s shutdown on context cancellation). It registers three
routes on a plain `net/http.ServeMux` — no router library, no middleware
framework:

```go
mux.Handle("/", http.FileServer(http.FS(sub)))   // sub = go:embed static/*
mux.HandleFunc("/api/topology", s.handleTopology)
mux.HandleFunc("/healthz", ...)
```

`handleTopology` is the only handler with any real logic. In order:

1. Impose a **30-second context deadline** on the whole request (`context.WithTimeout`).
2. If `RequireAuth`: extract the user from `X-Forwarded-User` /
   `X-Forwarded-Preferred-Username` / `X-Forwarded-Groups` (set by oauth-proxy);
   `401` if absent. Construct a per-request `AccessChecker` backed by the
   **shared, cross-request** `authzCache`.
3. Acquire a slot from `buildSem` (capacity 2) — or fail fast with `503` if the
   request's own deadline expires first while waiting.
4. Call `topology.Builder.Build(ctx)`.
5. Stream the result as indented JSON.

```go
builder := &topology.Builder{
	Client:     s.Client,
	States:     s.States,
	PolicyName: s.PolicyName,
	Cache:      s.cache,       // shared, cross-request rcache (10s TTL)
}
if s.RequireAuth {
	builder.Authz = NewAccessChecker(s.Client, user, s.authzCache) // 20s TTL
}
```

A **new `Builder` and `AccessChecker` struct are constructed per request**, but
they both point at the *same, long-lived* `Server.cache` / `Server.authzCache`
instances — that's the whole trick behind making repeated polls fast (see
below).

---

## Authentication & authorization

Two distinct concerns, both handled outside `topology.Builder` itself:

### 1. Authentication — "who is this?"

Delegated entirely to the **oauth-proxy sidecar**, which speaks OpenShift's
OAuth flow (`--provider=openshift`):

```
Browser                oauth-proxy                 OpenShift OAuth server
  │  GET / (no session cookie)  │                            │
  │─────────────────────────────▶                            │
  │  302 → OAuth login page      │                            │
  │◀─────────────────────────────│                            │
  │  user authenticates ─────────┼───────────────────────────▶│
  │  302 callback w/ code ◀──────┼────────────────────────────│
  │─────────────────────────────▶│ exchange code for token,   │
  │                               │ set signed session cookie  │
  │  subsequent requests: cookie only, no re-login             │
  │─────────────────────────────▶│ decode cookie, look up      │
  │                               │ identity, inject headers:   │
  │                               │  X-Forwarded-User            │
  │                               │  X-Forwarded-Groups           │
  │                               │───────────────▶ manager (webui.Server)
```

The manager never sees a password or token — only the already-validated
`X-Forwarded-*` identity headers (`internal/webui/authz.go`'s
`userFromRequest`). `--pass-access-token=false` in the sidecar's args means the
manager can't even see the user's OpenShift token if it wanted to.

### 2. Authorization — "what can they see?"

Every single node in the graph — every Gateway, Route, Service, and Pod (and
`IPAddressPool`, cluster-scoped) — is independently checked with a
`SubjectAccessReview` before being included, via the `topology.Authorizer`
interface:

```go
type Authorizer interface {
	Allowed(ctx context.Context, verb, group, resource, namespace, name string) bool
}
```

`internal/webui.AccessChecker` is the only implementation, issuing a real SAR
as the **operator's own ServiceAccount** (which has cluster-wide `create` on
`subjectaccessreviews` — RBAC evaluates the *subject in the SAR spec*, i.e. the
end user, not the ServiceAccount making the API call):

```go
sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
	User: user.Name, Groups: user.Groups,
	ResourceAttributes: &authzv1.ResourceAttributes{Verb, Group, Resource, Namespace, Name},
}}
```

A cluster-admin passes every check and sees the whole graph; an ordinary user
only sees what they can actually `get`. **Pools are a deliberate exception**:
a pool is shown (marked `restricted: true`, no console link) if it backs a
Gateway the user can already see, even without direct `IPAddressPool` read
access — otherwise the whole hierarchy would collapse to nothing for the
common case of a user who owns Gateways but not MetalLB config.

This is, by object count, the **single largest source of live API calls** in
the whole system — hundreds of Gateways × several objects each = potentially
thousands of SARs per full rebuild — which is why it has its own cache with
its own (longer) TTL; see [Caching architecture](#caching-architecture).

---

## Building the topology graph

`topology.Builder.Build(ctx)` (`internal/topology/builder.go`) is the single
entry point that both the dashboard **and, in spirit, the reconciler's own
`internal/trace.Resolver`** conceptually mirror (they are separate
implementations of the same Gateway→Route→Service→Pod trace — the builder
additionally computes UI-facing status rollups and honors `Authorizer`).

```
Build(ctx)
 │
 ├─ 1. loadPolicy()            — GatewayHealthPolicy (spec + shared status)
 ├─ 2. List IPAddressPools      (namespace = spec.metallb.namespace)
 ├─ 3. List all Gateways
 ├─ 4. indexRoutes()            — list HTTPRoute/GRPCRoute/TCPRoute/TLSRoute
 │                                 ONCE, group by target Gateway (avoids
 │                                 N separate route lists per Gateway)
 ├─ 5. For each Gateway, IN PARALLEL (≤ gatewayBuildConcurrency = 20):
 │       canRead(gateway)?  → skip if not
 │       buildGatewayNode():
 │         ├─ findProxyService()      (cached)
 │         ├─ proxyReplicas()         (cached — proxy Deployment scale)
 │         └─ for each attached Route:
 │              canRead(route)?
 │              for each backend Service ref:
 │                canRead(service)?
 │                getService()                    (cached)
 │                ├─ Skupper-labeled? → skupper.EvaluateListener() (remote leaf)
 │                └─ else podsForService():
 │                       List EndpointSlices        (cached)
 │                       canRead(pod)? getPod() ×N  (cached)
 │                     → health.EvaluatePod() per pod → roll up Service status
 │            → roll up Route status → roll up Gateway health+advertisement
 ├─ 6. Group resolved Gateways under the IPAddressPool that owns their VIP
 │       (unmatched → UnpooledGateways)
 ├─ 7. Roll up VIP status, then Pool status (worst-child, see HEALTH.md)
 └─ 8. summarize()              — aggregate counts for the toolbar
```

Step 5's parallelism is implemented with a bounded worker pattern — a
pre-sized result slice (so ordering is identical to a sequential loop) plus a
semaphore channel:

```go
results := make([]gwResult, len(gwList.Items))
sem := make(chan struct{}, gatewayBuildConcurrency)
for i := range gwList.Items {
	sem <- struct{}{}
	go func(i int, gw *gwapiv1.Gateway) {
		defer func() { <-sem }()
		if !canRead(gw) { return }
		results[i] = gwResult{node: buildGatewayNode(...), ok: true}
	}(i, gw)
}
wg.Wait()
```

Each Gateway's own trace is still **sequential internally** (its Routes'
Services' Pods are resolved one after another) — only the *outer* loop over
Gateways is parallelized. This is safe because each goroutine only reads
shared, already-fully-populated inputs (`routesByGateway`, `snaps`) and writes
to its own private slice index — no locks needed for the graph itself (the
shared *cache* underneath is separately concurrency-safe; see below).

---

## The data model

The JSON returned by `/api/topology` mirrors the physical hierarchy exactly:

```
Graph
 ├─ summary                      (toolbar counts)
 ├─ pools[]              IPAddressPool (MetalLB)
 │   └─ ips[]            VIP
 │       └─ gateways[]   Gateway (gateway.networking.k8s.io)
 │           └─ routes[] HTTPRoute / GRPCRoute / TCPRoute / TLSRoute
 │               └─ services[]  backend Service
 │                   └─ pods[]  backend workload Pod
 └─ unpooledGateways[]   Gateway  (VIP not sourced from any MetalLB pool)
```

Every node carries a normalized `status` field (`Healthy` / `Degraded` /
`Unhealthy` / `Withdrawn` / `Pending` / `Exempt` / `Unknown`) plus a `ref`
(group/version/kind/namespace/name) used to build OpenShift console deep
links, and an inlined `statusSince` / `statusForSeconds` pair. **The full
semantics of each status and how it rolls up from children are documented in
[`HEALTH.md`](HEALTH.md)** — this document only covers the JSON shape and how
it's produced/transported, not what each color means.

`Summary` (`internal/topology/model.go`) holds only coarse entity-type counts
(`pools`, `gateways`, `routes`, `services`, `pods`, `advertisedIPs`,
`withdrawnIPs`, `unhealthyGateways`) — it has **no per-status breakdown**. The
dashboard's clickable status-filter legend (see below) computes its own live
per-status counts client-side by walking the full graph, rather than requiring
a server-side change for every new UI affordance.

`SchemaVersion` (`"v1"`) is included so external consumers (e.g. a
multi-cluster hub) can detect breaking changes; the field is additive-friendly
by convention — new fields get added, existing ones don't change shape.

---

## The client: a static, dependency-free SPA

`internal/webui/static/` is `go:embed`'d whole (`//go:embed static/*`) into
the binary — there is no server-side templating engine and no client-side
build/bundle step (no webpack, no npm, no TypeScript compile). Three files:

| File | Role |
| --- | --- |
| `index.html` | Static shell: masthead, toolbar (`#summary`), status-filter legend (`#legend`), tree container (`#tree`), footer |
| `app.js` | All rendering and interactivity (vanilla ES6, one IIFE, ~560 lines) |
| `style.css` | Red Hat Design System theming; status → color mapping shared between the legend chips and every tree node's badge |

### Render pipeline

```
refresh()
  fetch("api/topology")
        │
        ▼
render(graph)
  lastGraph = graph                       (kept for re-render without re-fetch,
  renderSummary(graph.summary)             e.g. after collapse/expand or filter toggle)
  updateLegendUI()                         (live per-status counts + active-filter styling)
  graph.pools.map(poolNode)      ─┐
  graph.unpooledGateways.map(...)─┤
                                   ▼
      poolNode → ipNode → gatewayNode → routeNode → serviceNode → podNode
         (nodeRow() builds one DOM subtree per call; recursion mirrors the
          JSON hierarchy 1:1; collapse state and the active status filter
          are both applied during this same walk — see below)
```

Every node is built by a single shared helper, `nodeRow(opts)`, giving every
level of the tree identical structure: a toggle (▪/+), a kind badge, a
console-linked name, optional inline tags (e.g. `CRITICAL`), metadata, a
"for `<duration>`" age chip, and the colored status badge.

### Click-to-filter

The status legend doubles as a multi-select filter (added after the initial
build; see the "click-to-filter" feature). Clicking one or more chips
(`data-status="Unhealthy"`, etc.) toggles membership in a client-side
`statusFilter` `Set`; `render(lastGraph)` is re-run with no re-fetch. A node is
kept if **its own status matches the filter, or any descendant does** — so
filtering to `Unhealthy` still shows the Gateway/Service context around a
failing Pod, rather than an orphaned flat list:

```js
function serviceNode(s) {
  const kids = (s.pods || []).map(podNode).filter(Boolean);
  if (!statusVisible(s.status) && kids.length === 0) return null; // pruned
  ...
}
```

This filtering, plus collapse/expand state (`collapsed[id]`, keyed by a stable
per-node id like `"pod/<ns>/<name>"`), is recomputed on **every** render —
including every auto-refresh poll — so the UI stays visually stable across
polls even though the entire DOM subtree is rebuilt from scratch each time.

### Auto-refresh pacing (important — see the incident below)

```js
async function refresh() {
  if (refreshInFlight) return;   // re-entrancy guard
  refreshInFlight = true;
  try { ... } finally { refreshInFlight = false; }
}
function scheduleAuto() {
  if (timer) clearTimeout(timer);
  if (!autoEl.checked) return;
  timer = setTimeout(async () => { await refresh(); scheduleAuto(); }, 5000);
}
```

The next poll is scheduled **5 seconds after the previous one *finishes***,
not on a fixed wall-clock cadence — see
[Why it's built this way](#why-its-built-this-way-a-brief-performance-history)
for why that distinction matters a lot in practice.

---

## Caching architecture

There are **four independent layers** that work together to keep the
dashboard fast without ever holding an unbounded amount of cluster state in
memory. Understanding why there are four (not one) requires understanding what
each one does *not* cover:

```
┌────────────────────────────────────────────────────────────────────────┐
│ 1. Client re-entrancy guard (app.js)                                    │
│    Prevents ONE browser tab from ever having >1 request in flight.      │
├────────────────────────────────────────────────────────────────────────┤
│ 2. Server build semaphore  (webui.Server.buildSem, capacity 2)          │
│    Bounds concurrent full-graph rebuilds across ALL tabs/users — the    │
│    backstop for everything layer 1 can't see (multiple tabs, multiple   │
│    users, a manual refresh click racing an auto-refresh tick).          │
├────────────────────────────────────────────────────────────────────────┤
│ 3. Data cache  (topology.Builder.Cache, rcache, TTL = 10s)              │
│    De-duplicates repeated live Get/List calls for the SAME              │
│    Pod/Service/Deployment/EndpointSlice objects across polls.           │
├────────────────────────────────────────────────────────────────────────┤
│ 4. Authorization cache  (webui.Server.authzCache, rcache, TTL = 20s)    │
│    De-duplicates repeated SubjectAccessReview checks for the SAME       │
│    (user, verb, resource) tuples across polls. Separate from #3         │
│    because SAR results and object data warrant different TTLs, and      │
│    live on a completely different code path (authz.go, not builder.go).│
└────────────────────────────────────────────────────────────────────────┘
```

### Why #3 and #4 exist at all: `Client.Cache.DisableFor`

By default, a controller-runtime manager's client serves **all** reads
through a persistent, watch-based informer cache — but that cache holds
*every object of a type in the entire cluster*, forever, once anything reads
that type once. On a large or shared cluster, an unscoped watch on Pods (or
Services, or Deployments) is exactly what once caused the controller manager
itself to be **OOMKilled**. The fix (see `cmd/main.go`) was to exclude those
three types from the informer cache entirely:

```go
Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{
	&corev1.Pod{}, &corev1.Service{}, &appsv1.Deployment{},
}}}
```

That trades unbounded memory for **every read becoming a live API call** —
which is exactly right for a reconciler (each Gateway reconcile only touches a
handful of objects), but directly at odds with a dashboard that rebuilds
*every* Gateway's full backend tree from scratch on *every* poll. `rcache`
(`internal/rcache/`) is the deliberate middle ground:

```
                    informer/watch cache          rcache (this package)
scope               entire GVK, cluster-wide       only keys actually requested
lifetime            forever (until process exit)   a few seconds (TTL)
memory bound        O(objects in cluster)           O(objects touched recently)
staleness           near-zero (watch-driven)        up to the TTL
```

`rcache.Cache` is intentionally tiny: a `map[string]entry{value any, expires
time.Time}` behind a mutex, with opportunistic expired-entry eviction on every
`Set` (bounded to 32 entries swept per call, so `Set` itself stays cheap even
if the map grows large). A `nil *Cache` is a valid, always-miss no-op, so
callers never need to special-case "caching disabled."

### TTL choices, and why they must exceed the poll gap

| Cache | TTL | Rationale |
| --- | --- | --- |
| `topologyCacheTTL` | **10s** | Must exceed the *actual* gap between polls — which is `5s + however long the last build took`, not a flat 5s (see the self-rescheduling `setTimeout` above). A TTL shorter than that gap means every "cached" poll is actually cache-cold; this was tuned up from an initial 4s after measuring exactly that. |
| `authzCacheTTL` | **20s** | RBAC bindings change far less often than pod health, so a longer TTL is safe. The one trade-off: a just-revoked permission can remain visible in the dashboard for up to 20s — the underlying Kubernetes API itself is unaffected, this only controls what Beacon's own dashboard chooses to *display*. |

Neither cache affects the **reconciler's** own health/advertisement decisions
— those go through a completely separate, uncached path
(`internal/trace.Resolver`), so withdraw/re-advertise timing is unaffected by
anything in this document.

---

## Concurrency & rate limiting

Caching alone wasn't sufficient — two more fixes were needed to get a
cold/cache-miss build to a reasonable latency on a cluster with hundreds of
Gateways:

1. **`gatewayBuildConcurrency = 20`** — see
   [Building the topology graph](#building-the-topology-graph). Converts
   `Build()`'s wall-clock cost from *sum of every Gateway's trace time* to
   roughly *one Gateway's trace time × (Gateways / 20)*.
2. **Raised client-go QPS/Burst** (`cmd/main.go`) — client-go defaults a REST
   client to a conservative **5 QPS / 10 burst**
   (`k8s.io/client-go/rest.DefaultQPS/DefaultBurst`). That's invisible when
   reads come from a warm informer cache, but once Pod/Service/Deployment
   became live calls, *every* goroutine from #1 funneled through the same
   process-wide 5-requests-per-second ceiling — concurrency alone barely
   helped until this was raised:

   ```go
   restCfg := ctrl.GetConfigOrDie()
   restCfg.QPS = 100
   restCfg.Burst = 200
   ```

   The API server's own Priority & Fairness (APF) still protects it from any
   single client regardless of this setting.

Both of these apply to **every** client of the shared manager `Client` —
reconciler included — not just the dashboard.

---

## Multi-replica consistency

The dashboard runs on every replica (no leader election), but only the
**leader** replica's `GatewayReconciler` actually evaluates health and drives
withdraw/re-advertise decisions. A non-leader replica's in-memory
`state.Store` (`internal/state/`) is therefore always empty. To keep both
replicas' dashboards showing identical data regardless of which one a request
lands on, `topology.Builder` prefers the **shared, CRD-persisted** status over
the local in-memory store:

```
Replica A (leader)                              Replica B (standby)
┌─────────────────────────┐                     ┌─────────────────────────┐
│ GatewayReconciler         │                     │ GatewayReconciler        │
│  evaluates health,        │                     │  idle (no leader lease)  │
│  drives advertise/withdraw│                     │                          │
│  writes state.Store (local, only used as a     │  state.Store: empty      │
│  same-replica fallback)   │                     │                          │
│  patches                  │                     │                          │
│  GatewayHealthPolicy.status│                    │                          │
└────────────┬──────────────┘                     └────────────┬─────────────┘
             │                                                  │
             ▼                                                  │
     GatewayHealthPolicy CR (shared, cluster-wide)               │
             │◀────────────────── both replicas read this first ┘
             ▼                                                  ▼
     webui.Server (replica A)                          webui.Server (replica B)
       topology.Builder prefers                           topology.Builder prefers
       pol.Status.Gateways;                                pol.Status.Gateways;
       falls back to state.Store                            falls back to state.Store
       only if the CR status is empty                       (normally empty on a
       (e.g. brand new install)                              standby — but doesn't matter,
                                                              the shared status is used)
```

```go
snaps := snapshotsFromPolicyStatus(pol)
if len(snaps) == 0 && b.States != nil {
	snaps = b.States.Snapshot()
}
```

A user hitting the dashboard Service (which load-balances across both pods)
sees the same advertisement/health state either way. The only thing
"local-only" ever affects is the extremely narrow window right after a fresh
install, before the leader has reconciled anything into the shared status yet.

---

## Why it's built this way: a brief performance history

The caching/concurrency architecture above wasn't the starting design — it's
the result of two real incidents, kept here because the reasoning explains
several otherwise-non-obvious constants in the code:

1. **OOM crash loop.** The controller manager was being `OOMKilled` on a
   large, shared test cluster. Root cause: unscoped `Watches()` on
   Pod/Service/Deployment/EndpointSlice made controller-runtime's informer
   cache hold *every* object of those types *cluster-wide*, forever — memory
   that scaled with total cluster size, not with what Beacon actually
   manages. Fix: `Client.Cache.DisableFor` those three types (EndpointSlice
   stayed cached — far lower cardinality, and needed as the reactive trigger
   for backend readiness changes) plus dropping their `.Watches()`
   registrations. This traded unbounded memory for live-per-read API calls.

2. **Dashboard became unusably slow (and *worse* on refresh).** Once reads
   were live, the dashboard — which rebuilds the *entire* graph, for
   *every* Gateway, on *every* poll — started taking 20–30+ seconds per
   load, sometimes hitting the 30s handler timeout outright
   (`context deadline exceeded`, visible as `http: superfluous
   response.WriteHeader` in the logs). Worse, it got *progressively slower*
   on repeated refreshes: `app.js` originally used `setInterval(refresh,
   5000)`, which fires regardless of whether the previous fetch finished. A
   single slow request meant the next tick fired an *overlapping* one, each
   spawning its own full rebuild, all competing for the same rate-limited
   client and getting slower still — a thundering-herd pileup that compounds
   without bound. The fix was every mechanism described in this document:
   bounded per-Gateway concurrency, raised client QPS/Burst, the two `rcache`
   layers, the server-side build semaphore, and switching the client's
   auto-refresh from a fixed `setInterval` to a self-pacing, re-entrant-safe
   `setTimeout` chain.

The net effect, measured directly against a cluster with ~168 Gateways: a
cold dashboard load went from **26s+ (routinely timing out)** to **~0.6s**,
and steady-state auto-refresh polls (cache-warm) dropped to **~0.05s** —
several hundred times faster, with memory usage unchanged from the
already-fixed OOM baseline (well under the container's 128Mi limit).

---

## Security considerations

- **Read-only.** The dashboard never mutates cluster state; it only reads and
  displays. Advertise/withdraw actions are exclusively driven by the
  reconciler, independent of anyone viewing the dashboard.
- **Per-object RBAC-backed filtering**, not a blanket "authenticated users see
  everything" model — see [Authentication & authorization](#authentication--authorization).
- **Cache staleness is a display-only concern.** Both `rcache` layers only
  affect what Beacon's *own dashboard* chooses to show for up to their TTL
  (10s / 20s); they never bypass or cache actual Kubernetes API
  authorization decisions — every cache miss still issues a real, fresh SAR
  or object read, subject to the cluster's real RBAC at that moment.
- **The manager never handles user credentials or tokens.** oauth-proxy
  terminates the entire OAuth flow and forwards only an already-validated
  identity (username + groups) via headers; `--pass-access-token=false`.
- **Unauthenticated mode is explicitly opt-in and documented as dev-only**
  (`--dashboard-auth-required=false`, only wired up by `make deploy`'s
  kustomize path, never by the OLM/CSV install).

---

## File map

| Path | Responsibility |
| --- | --- |
| `internal/webui/server.go` | HTTP server, routing, request-scoped wiring, the two `rcache` instances, the build semaphore |
| `internal/webui/authz.go` | `X-Forwarded-*` header parsing, `AccessChecker` (SubjectAccessReview + cache) |
| `internal/webui/dashboard_resources.go` | Provisions Service/Route/ConsoleLink/Secrets/SA-annotation at startup |
| `internal/webui/static/index.html` | Static HTML shell |
| `internal/webui/static/app.js` | All client-side rendering, filtering, and auto-refresh logic |
| `internal/webui/static/style.css` | Red Hat Design System theming, status → color mapping |
| `internal/topology/model.go` | The `Graph` JSON schema (Go structs with `json` tags) |
| `internal/topology/builder.go` | `Build()` — the graph construction pipeline, concurrency, per-object caching wrappers |
| `internal/topology/status.go` | Status rollup logic (severity ordering, worst-child folding) — see `HEALTH.md` for the semantics |
| `internal/rcache/rcache.go` | The generic short-TTL cache used by both #3 and #4 above |
| `internal/state/store.go` | Per-replica in-memory Gateway snapshot store (leader-populated, fallback-only for the dashboard) |
| `cmd/main.go` | Manager wiring: `Client.Cache.DisableFor`, QPS/Burst, dashboard flags, `webui.NewServer` construction |
| `config/rbac/dashboard_role.yaml` | Namespaced Role for the Secrets/ServiceAccount the dashboard's oauth-proxy setup needs |
| `config/dashboard/` | Kustomize-path (unauthenticated) Service/Route, for `make deploy` |
