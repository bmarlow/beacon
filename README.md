<p align="center">
  <img src="docs/assets/beacon-logo.png" alt="Beacon logo" width="160" height="160" />
</p>

# Beacon

**Beacon exists because the Gateway API has no native understanding of the
actual health of the backend services behind a Gateway.** A Gateway keeps
advertising its load-balancer IP as long as the Gateway and its proxy exist —
even when the workloads it routes to are failing their health probes (or, for
cross-cluster backends, are unreachable over a Skupper link). Upstream routers
happily keep steering traffic to a VIP whose backends are down.

**Beacon** is an OpenShift/Kubernetes operator that closes that gap by coupling
**real backend health** to **MetalLB** BGP route advertisements. It continuously
watches your Gateways, traces each one down to the pods (and Skupper-linked
remote services) that actually serve its traffic, and — based on those backends'
health probes — withdraws or restores the Gateway's load-balancer IP in MetalLB.
All of this happens **without restarting MetalLB or flapping BGP adjacencies**.

Think of it as a health-aware route dampener that keeps your BGP-advertised
Gateway VIPs pointed only at healthy backends — the backend-health awareness the
Gateway API itself doesn't provide.

---

## Table of contents

- [Why Beacon?](#why-beacon)
- [How it works](#how-it-works)
  - [Tracing a Gateway to its pods](#tracing-a-gateway-to-its-pods)
  - [Health rules](#health-rules)
  - [Multiple backend services per Gateway](#multiple-backend-services-per-gateway)
  - [Withdrawing without flapping BGP (proxy-drain)](#withdrawing-without-flapping-bgp-proxy-drain)
  - [Dampening state machine](#dampening-state-machine)
- [Skupper-linked (remote) backends](#skupper-linked-remote-backends)
- [Prerequisites](#prerequisites)
- [Installation](#installation)
  - [Install via OLM (OperatorHub)](#install-via-olm-operatorhub)
  - [Install with kustomize](#install-with-kustomize)
  - [Run locally for development](#run-locally-for-development)
- [Usage](#usage)
  - [The GatewayHealthPolicy resource](#the-gatewayhealthpolicy-resource)
  - [Configuring MetalLB for Beacon](#configuring-metallb-for-beacon)
  - [Exempting gateways](#exempting-gateways)
  - [Per-gateway timer overrides](#per-gateway-timer-overrides)
  - [Pausing the operator](#pausing-the-operator)
- [Topology dashboard (web UI)](#topology-dashboard-web-ui)
  - [Authentication & per-user access](#authentication--per-user-access)
  - [Finding the dashboard](#finding-the-dashboard)
- [Observability](#observability)
- [Configuration reference](#configuration-reference)
- [Architecture](#architecture)
- [Development](#development)
- [FAQ / troubleshooting](#faq--troubleshooting)
- [License](#license)

---

## Why Beacon?

MetalLB in BGP mode advertises a Service's load-balancer IP to upstream routers
as long as the Service exists and has endpoints. But "has endpoints" is a coarse
signal: a pod can be `Running` and part of a Service's endpoints while its
application is actually failing its readiness/liveness probes in ways MetalLB
never sees. In that window, the router keeps sending traffic to a VIP whose
backends are unhealthy.

Beacon closes that gap for **Gateway API** Gateways specifically:

- It understands the Gateway → Service → Pod → probe chain.
- It withdraws the VIP's route the moment the backing workload is genuinely
  unhealthy (after a short dampening delay), so upstream routers stop steering
  traffic to a black hole.
- It restores the route once the workload recovers.
- It never bounces the BGP session, so unrelated prefixes and neighbors are
  unaffected.
- It also covers **Skupper-linked backends** — if the real workload lives on a
  remote cluster, Beacon reacts to that remote workload's health too (see
  [Skupper-linked (remote) backends](#skupper-linked-remote-backends)).

---

## How it works

### Tracing a Gateway to its pods

A Gateway API deployment has **two distinct pod populations**, and it is
essential to monitor the right one:

- **Proxy / data-plane pods** — the ingress pods (envoy, istio-ingressgateway,
  nginx) that the Gateway's own `LoadBalancer` Service selects and that MetalLB's
  endpoints point at. MetalLB already reacts to these natively.
- **Backend / workload pods** — the application pods that the `HTTPRoute`,
  `GRPCRoute`, `TCPRoute`, and `TLSRoute` resources attached to the Gateway
  forward traffic to. **These are the pods whose health decides whether the VIP
  should be advertised.**

Beacon monitors the **backend pods**. For every `Gateway` it:

1. **Infers the load-balancer IP(s)** from `Gateway.status.addresses`, falling
   back to the proxy `Service`'s `status.loadBalancer.ingress`.
2. **Identifies the proxy `Service` and its `Deployment`** — the `LoadBalancer`
   Service the Gateway implementation created (via the
   `gateway.networking.k8s.io/gateway-name` label, or by matching ingress IPs)
   and the proxy `Deployment` behind it. These are used **only** as the VIP and
   as the withdrawal lever (see below) — never as a source of health pods.
3. **Determines whether the IP is from MetalLB** by matching it against the
   CIDRs/ranges of every `IPAddressPool` in the MetalLB namespace. IPs that are
   **not** from MetalLB are observed but never mutated.
4. **Discovers the backend `Service`(s)** by listing every xRoute whose
   `parentRefs` attach it to this Gateway and collecting the `backendRefs`
   (core `Service` kind, including cross-namespace refs).
5. **Traces each backend Service to its pods** via `EndpointSlice`s (falling
   back to the Service selector), then inspects each pod's containers' probes.
   If a backend is a **Skupper Listener** (a remote, off-cluster workload),
   Beacon evaluates the Listener's status instead — see [Skupper-linked (remote)
   backends](#skupper-linked-remote-backends).

The full chain:

```
Gateway
  ├─ (VIP)  Gateway.status.addresses  ||  proxy LoadBalancer Service ingress IP
  └─ (health) xRoutes whose parentRefs -> this Gateway
                └─ rule.backendRefs -> backend Service(s)   (any namespace)
                     └─ EndpointSlices -> backend workload Pods  <-- probed
```

### Health rules

Beacon's health contract is deliberately simple and predictable:

| Pod condition                                             | Treatment                    |
| -------------------------------------------------------- | ---------------------------- |
| No readiness/liveness/startup probe on any container     | **Exempt** — ignored         |
| Has a probe **and** `PodReady=True`                       | Healthy                      |
| Has a probe **and** `PodReady!=True`                      | Unhealthy                    |
| Terminating (`deletionTimestamp` set)                    | Ignored (transient)          |

**A backend Service is up** based on the percentage of its probed pods that are
Ready — see [Multiple backend services per Gateway](#multiple-backend-services-per-gateway).
By **default a single Ready pod keeps the Service up** (`minHealthyPodPercent: 1`),
so a Service with 1 of 3 pods Ready is still considered healthy. Raise the
threshold (per Gateway or per Service) to require more replicas.

A **Gateway** is then:

- **Unhealthy** when the percentage of its healthy backend Services drops below
  `minHealthyBackendPercent` (default 100 = any counted backend down).
- **Exempt** if every backend is exempt (no probed pods / no counted backends).
- **Healthy** otherwise.

> Only a Gateway that is **Unhealthy** and whose IP is **sourced from MetalLB**
> is eligible for withdrawal.

> A backend can also be a **Skupper-linked remote workload** (running on another
> cluster). Beacon health-checks those via the Skupper `Listener` status rather
> than local pods — see [Skupper-linked (remote)
> backends](#skupper-linked-remote-backends).

### Multiple backend services per Gateway

Beacon evaluates health at **two levels**, both configurable:

**1. Is a backend Service up? — `minHealthyPodPercent`** (per-Service).
A Service is healthy while the percentage of its probed pods that are Ready meets
the threshold (inclusive): `(readyPods / probedPods) * 100 >= minHealthyPodPercent`.

- **Default `1` — a single Ready pod keeps the Service up.** So 1 of 3 pods Ready
  → the Service is still healthy (Kubernetes routes around the NotReady pods).
- Raise it to require more replicas: `100` = all pods must be Ready; `50` = at
  least half.
- Configurable at the **Gateway** level (`spec.minHealthyPodPercent` or the
  `beacon.io/min-healthy-pod-percent` annotation on the Gateway) **and overridable
  per backend Service** via the same annotation on the Service. Precedence:
  **Service annotation > Gateway annotation > policy default**.
- Probe-less pods are ignored; a Service with no probed pods is exempt.

> **Scaled-to-zero backends — `zeroReplicasPolicy`.** A Service that has a pod
> selector but **zero pods** (scaled to zero) has no pod health signal. By
> default (`zeroReplicasPolicy: Unhealthy`) Beacon treats it as a **counted,
> failing** backend, so an accidental scale-to-zero lowers the healthy ratio and
> can withdraw the VIP instead of leaving a black hole. Set `Exempt` to tolerate
> deliberate scale-to-zero (the Service is simply ignored). Selector-less
> Services (e.g. `ExternalName`) are always exempt. Configurable in the policy
> (`spec.zeroReplicasPolicy`) and overridable per-Gateway or per-Service via the
> `beacon.io/zero-replicas-policy` annotation (Service > Gateway > policy).

**2. Is the Gateway up? — `minHealthyBackendPercent`** (per-Gateway).
Given each Service's up/down verdict from level 1, this is the minimum percentage
of a Gateway's *counted* backend services that must be healthy for the route to
stay advertised. It is evaluated **inclusively**:

```
stay advertised  while  (healthy backends / counted backends) * 100  >=  minHealthyBackendPercent
withdraw         when   the ratio drops below the threshold
```

- **Counted** backends are those with a health signal — services with probed
  pods, or Skupper-linked services. Probe-less/exempt services are excluded from
  the ratio.
- A backend service is **healthy** when all its probed pods are ready (or, for a
  Skupper backend, the link is `Ready`).

Example — `minHealthyBackendPercent: 50` on a Gateway with **4** backend
services:

| Backends down | Healthy % | Result |
| ------------- | --------- | ------ |
| 0 or 1        | 100% / 75% | advertised (Healthy / Degraded) |
| 2             | **50%**    | advertised — 50% meets the inclusive 50% threshold (Degraded) |
| 3             | 25%        | **withdrawn** (below 50%) |
| 4             | 0%         | **withdrawn** |

Set it globally in the policy (`spec.minHealthyBackendPercent`, default `100`) or
per-Gateway via the `beacon.io/min-healthy-percent` annotation. The dashboard
shows each Gateway's `backends healthy/counted (min N%)`. When the threshold is
crossed, the usual dampening + proxy-drain withdraw/re-advertise applies.

> **Note:** a Gateway that is **Degraded** (some backends down but still at/above
> the threshold) remains **advertised**; only **Unhealthy** (below threshold)
> triggers withdrawal.

### Withdrawing without flapping BGP (proxy-drain)

This is the core safety property, and it works **with** MetalLB's design rather
than against it.

**Why not manipulate MetalLB CRs?** It is tempting to withdraw a route by
editing `BGPAdvertisement`/`IPAddressPool` objects, but this does not work
reliably. Verified against a live MetalLB (BGP mode) deployment:

- `BGPAdvertisement` has **no per-Service selector** — you cannot scope an
  advertisement to a single Service.
- MetalLB **forbids overlapping pool CIDRs**, so you cannot carve a dedicated
  `/32` pool out of an existing range that already has assigned IPs.
- MetalLB **deliberately keeps advertising** on disruptive config changes: if
  you remove a pool/advertisement backing an assigned IP, MetalLB marks the
  config *stale* and retains the last-good advertisement to preserve
  connectivity.

**What MetalLB does support**, natively and gracefully, is withdrawing a
Service's route when that Service has **zero ready endpoints**. It sends a single
BGP **withdraw** for just that prefix over the **existing** session — the
adjacency never flaps and no other prefixes are disturbed.

**How Beacon uses it.** A Gateway's data plane is a `Deployment` (e.g. the
Istio-managed ingress gateway) selected by
`gateway.networking.k8s.io/gateway-name=<gateway>`. Beacon drains the proxy's
endpoints by scaling that Deployment:

- To **withdraw**: Beacon scales the proxy `Deployment` to **0 replicas**
  (recording the previous count in the `beacon.io/saved-replicas` annotation and
  marking `beacon.io/withdrawn=true`). The proxy `Service` then has no ready
  endpoints, and MetalLB withdraws the VIP.
- To **restore**: Beacon scales the proxy `Deployment` back to its saved replica
  count. The proxy returns, endpoints become ready, and MetalLB re-announces the
  VIP.

The BGP session is never torn down, so **no adjacency down/up event occurs** and
other Gateways' VIPs are unaffected. (Validated on-cluster: withdrawing one VIP
left the other VIPs advertised and all BGP sessions `Established`.)

> **Granularity & requirements.** Because the lever is the Gateway's proxy
> Deployment, withdrawal is **whole-Gateway**: every VIP/route fronted by that
> Gateway is withdrawn together. This is exactly right for the common
> one-Gateway-per-VIP topology. It also requires the Gateway services to use
> `externalTrafficPolicy: Cluster` (so per-node endpoint churn does not cause
> advertisement flap) and that the Gateway implementation does not immediately
> reconcile the proxy Deployment's replica count back to a fixed value
> (Istio/OpenShift gateways honor manual/HPA scaling — verified). Recovery
> incurs a proxy cold-start before re-advertisement.

### Dampening state machine

To avoid route flap on transient blips, Beacon applies dampening timers before
acting:

```
                 unhealthy >= withdrawAfter
   ┌───────────┐ ─────────────────────────▶ ┌───────────┐
   │ Advertised│                             │ Withdrawn │
   └───────────┘ ◀───────────────────────── └───────────┘
                 healthy >= readvertiseAfter

   Intermediate states while a timer is counting:
     Advertised  --unhealthy--> PendingWithdrawal  --(timer)--> Withdrawn
     Withdrawn   --healthy---->  PendingReadvertise --(timer)--> Advertised
```

- **`withdrawAfter`** (default **5s**): the workload must be *continuously*
  unhealthy this long before the route is withdrawn.
- **`readvertiseAfter`** (default **30s**): the workload must be *continuously*
  healthy this long before the route is restored.

Any recovery during `PendingWithdrawal` cancels the withdrawal; any relapse
during `PendingReadvertise` cancels the restore. This asymmetric timing (fast to
withdraw, slow to restore) favors traffic correctness while damping oscillation.

---

## Skupper-linked (remote) backends

Beacon understands backends whose real workload lives on **another cluster**,
reached over a [Skupper](https://skupper.io/) link. This lets a Gateway VIP be
withdrawn when the *remote* workload fails — the same way it would for a local
one.

### Why this needs special handling

A Skupper `Listener` creates a **local Service** for the remote workload, but
that Service's endpoints point at the local **`skupper-router`** pod, not the
remote application. The router is essentially always healthy locally, so:

- ordinary pod-probe tracing sees "endpoints exist, router is Ready" and would
  consider the backend healthy, and
- a failure on the remote cluster (crash, scale-to-zero, broken link) would
  **never** be reflected in the VIP's advertisement.

### How Beacon detects and evaluates them

Beacon recognizes a Skupper-backed Service by the label Skupper stamps on it:

```
internal.skupper.io/listener: <listener-name>
```

For such a Service it evaluates the **Skupper `Listener`'s status** (in the
Service's namespace) instead of local pods:

| Listener state | Meaning | Beacon treats backend as |
| -------------- | ------- | ------------------------ |
| `status: Ready` / `Ready=True` (a matching remote `Connector` exists, link operational) | remote workload reachable | **Healthy** |
| `status: Pending` / `Ready=False`, e.g. *"No matching connectors"* (remote workload down, scaled to zero, or link broken) | remote workload unavailable | **Unhealthy** |
| Listener object missing | can't resolve the remote backend | **Unhealthy** (fail-safe) |
| Skupper CRDs not installed | not a Skupper cluster | ignored (never marks unhealthy) |

The result folds into the Gateway's aggregate health **exactly like a local
probed pod**, so the standard behavior applies unchanged:

- remote workload fails → after `withdrawAfter`, Beacon drains the Gateway's
  proxy and MetalLB **withdraws** the VIP;
- remote workload recovers → after `readvertiseAfter`, Beacon restores the proxy
  and MetalLB **re-advertises** the VIP.

### In the dashboard

Skupper-backed backends render as **Service (Skupper)** with a 🔗 link-status
indicator (`remote ready` / `remote unavailable`) and a synthetic **Remote**
leaf standing in for the off-cluster workload (there is no local pod to show).

### Requirements & notes

- The operator needs read access to `skupper.io/listeners` (granted by the
  bundle RBAC). Clusters without Skupper are unaffected — the check is skipped
  when the CRD is absent.
- Tested against **Skupper v2** (`skupper.io/v2alpha1` `Listener`). The Gateway,
  MetalLB, and dampening requirements are otherwise identical to local backends.
- Only the `Listener`-side (this cluster consuming a remote service) is
  evaluated; Beacon does not need to run on, or reach, the remote cluster.

---

## Prerequisites

- OpenShift 4.14+ (or Kubernetes 1.27+).
- [Gateway API](https://gateway-api.sigs.k8s.io/) CRDs installed, with at least
  one Gateway implementation provisioning `LoadBalancer` Services.
- [MetalLB](https://metallb.io/) installed in **L3 / BGP mode** (the MetalLB
  Operator on OpenShift), with `IPAddressPool` and `BGPPeer` configured. Beacon
  supports **BGP mode only** — L2 (ARP/NDP) mode is out of scope (see
  [FAQ](#faq--troubleshooting)).
- *(Optional)* [Skupper](https://skupper.io/) v2 (`skupper.io/v2alpha1`) if any
  Gateway backends are remote services reached over a Skupper link — see
  [Skupper-linked (remote) backends](#skupper-linked-remote-backends). Not
  required otherwise.

---

## Container images

The operator image is built and published automatically by GitHub Actions
(`.github/workflows/build-image.yaml`) on **every push to `main`** (and on `v*`
tags) to **GitHub Container Registry**:

```
ghcr.io/bmarlow/beacon:latest        # rolling latest from main
ghcr.io/bmarlow/beacon:sha-<short>   # immutable, per-commit (recommended for rollouts)
ghcr.io/bmarlow/beacon:X.Y.Z         # published when a vX.Y.Z git tag is pushed
```

Pull requests build the image (to catch breakage) but do not push. For
reproducible deployments, pin the `sha-<short>` tag in your kustomize overlay
rather than `latest`.

> The ghcr.io package must be **public** for the cluster to pull it anonymously
> (Repository → Packages → beacon → Package settings → Change visibility), or
> configure an image pull secret referencing a token with `read:packages`.

---

## Installation

Beacon installs via **OLM** (recommended, so it appears under the console's
Installed Operators and manages its own lifecycle) or directly with kustomize.

### Install via OLM (OperatorHub)

If Beacon is published to a catalog, install through the console
(**Operators → OperatorHub → Beacon**) or with a `Subscription`.

To install the bundle directly (what the reference deployment uses), build/push
the bundle and run it into the `beacon` namespace:

```bash
# Build & push the bundle image (the operator image is built by CI on ghcr.io).
make bundle VERSION=0.1.0
make bundle-build bundle-push BUNDLE_IMG=ghcr.io/<you>/beacon-bundle:0.1.0

# Install via OLM (creates the OperatorGroup, CatalogSource, Subscription, CSV).
oc create namespace beacon
operator-sdk run bundle ghcr.io/<you>/beacon-bundle:0.1.0 \
  --namespace beacon --install-mode AllNamespaces

# Create the singleton policy.
kubectl apply -f config/samples/beacon_v1alpha1_gatewayhealthpolicy.yaml
```

> The operator requires **AllNamespaces** install mode (its CRD is
> cluster-scoped and it holds cluster-wide permissions — see the CSV
> "Cluster permissions & rationale"). It creates its own dashboard `Service`,
> `Route`, `ConsoleLink`, metrics `Service`, `ServiceMonitor`, `PrometheusRule`,
> and oauth-proxy auth resources at startup — no extra manifests needed.

**Upgrade approval.** For production, use a deliberate InstallPlan approval
strategy on the `Subscription` so cluster admins review each upgrade:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: beacon
  namespace: beacon
spec:
  channel: stable
  name: beacon
  source: <your-catalog-source>
  sourceNamespace: openshift-marketplace
  installPlanApproval: Manual   # admin approves each upgrade; use Automatic for auto-updates
```

### Install with kustomize

For non-OLM clusters (or development). Note the kustomize path deploys the
manager only; the oauth-proxy sidecar and dashboard wiring are fully specified
in the OLM CSV, so **for the authenticated dashboard use the OLM install**.

```bash
# Install the CRD.
make install

# Deploy the controller (config/default -> namespace beacon-system).
make deploy IMG=ghcr.io/<you>/beacon:latest

# Create the singleton policy.
kubectl apply -f config/samples/beacon_v1alpha1_gatewayhealthpolicy.yaml
```

### Run locally for development

Runs the controller on your machine against your current kubeconfig context:

```bash
make install          # install the CRD into the cluster
make run              # run the manager locally
```

---

## Usage

### The GatewayHealthPolicy resource

Beacon is configured by a single **cluster-scoped** `GatewayHealthPolicy` named
`cluster` (the name is configurable via the manager's `--policy-name` flag):

```yaml
apiVersion: beacon.io/v1alpha1
kind: GatewayHealthPolicy
metadata:
  name: cluster
spec:
  withdrawAfter: 5s        # continuous-unhealthy before withdraw (default 5s)
  readvertiseAfter: 30s    # continuous-healthy before restore   (default 30s)
  resyncInterval: 10s      # worst-case reconcile cadence         (default 10s)
  paused: false            # observe-only when true

  metallb:
    # Namespace where MetalLB's IPAddressPools live; used to determine which
    # Gateway VIPs are sourced from MetalLB.
    namespace: metallb-system

  # Optional: only manage these Gateway classes (empty = all).
  gatewayClassNames:
    - openshift-default

  # Optional: exempt specific gateways (in addition to the annotation).
  exemptions:
    - namespace: infra
      name: management-gateway
```

Check status at a glance:

```bash
$ kubectl get gatewayhealthpolicy cluster
NAME      MANAGED   ADVERTISED   WITHDRAWN   PAUSED   AGE
cluster   7         6            1           false    3h
```

### Configuring MetalLB for Beacon

Beacon does **not** create or modify any MetalLB `IPAddressPool` or
`BGPAdvertisement` objects. Your normal MetalLB configuration is used as-is —
Beacon only reads `IPAddressPool`s (to determine which Gateway VIPs are sourced
from MetalLB) and withdraws/restores a VIP by draining the Gateway's proxy (see
[Withdrawing without flapping BGP](#withdrawing-without-flapping-bgp-proxy-drain)).

Requirements for the withdrawal mechanism to work correctly:

- The Gateway's `LoadBalancer` Service uses `externalTrafficPolicy: Cluster`.
- MetalLB advertises the Gateway VIP normally (any standard pool +
  `BGPAdvertisement` + `BGPPeer`), so that removing the proxy's ready endpoints
  causes MetalLB to withdraw that VIP.

A typical MetalLB setup (unchanged by Beacon):

```yaml
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: gateways
  namespace: metallb-system
spec:
  addresses:
    - 192.0.2.0/24
---
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata:
  name: gateways
  namespace: metallb-system
spec:
  ipAddressPools:
    - gateways
---
apiVersion: metallb.io/v1beta2
kind: BGPPeer
metadata:
  name: upstream
  namespace: metallb-system
spec:
  myASN: 64512
  peerASN: 64513
  peerAddress: 192.0.2.254
```

### Exempting gateways

Two mechanisms, both honored:

1. **Per-gateway annotation** (GitOps-friendly, lives with the Gateway):

   ```yaml
   apiVersion: gateway.networking.k8s.io/v1
   kind: Gateway
   metadata:
     name: management-gateway
     annotations:
       beacon.io/exempt: "true"
   ```

2. **Central list** in the policy `spec.exemptions` (see example above).

Exempt Gateways are observed and reported as `Exempt` but never mutated.

### Per-gateway timer overrides

Individual Gateways can override the global dampening timers via annotations:

```yaml
metadata:
  annotations:
    beacon.io/withdraw-after: "10s"
    beacon.io/readvertise-after: "60s"
```

### Pausing the operator

Set `spec.paused: true` for a maintenance window. Beacon continues to observe
and report health/intended state but performs **no** MetalLB mutations.

---

## Topology dashboard (web UI)

Beacon serves a built-in web dashboard that renders the full relationship chain
**from the MetalLB IP all the way down to the app/service pod**, with live status
at every level:

```
IPAddressPool (MetalLB)          [name, CIDRs; time-in-status]
  └─ VIP  (Advertised / Withdrawn / Pending; time-in-status)
       └─ Gateway                (health, advertisement, class, replicas R/D,
            │                      dampening timer countdown, time-in-status)
            └─ Route             (HTTP/GRPC/TCP/TLS, hostnames)
                 └─ Service       (backend)
                      └─ Pod      (phase, probe status, node, time-in-status)
```

Highlights:

- Each node is **color-coded** (Healthy / Degraded / Unhealthy / Withdrawn /
  Pending / Exempt / Unknown); the tree is collapsible and auto-refreshes.
- Every component name is a **clickable link to its OpenShift console page**
  (opens in a new tab) when you have access to that object.
- Each node shows **how long it has been in its current status** (e.g. `for
  3m12s`).
- Gateway rows show the proxy **replica count** (`replicas R/D`) and, when a
  dampening timer is running, a live **countdown** — `backoff 2s / 5s (3s left)`
  or `recovery 12s / 30s (18s left)`.
- The header shows the **operator version**, aggregate counts, the **logged-in
  user**, and a **Log out** button.

### Authentication & per-user access

The dashboard is **authenticated by default** using OpenShift's OAuth. An
`oauth-proxy` sidecar (in the operator pod) fronts the UI: unauthenticated
requests are redirected to the OpenShift login, and only the authenticated
user's identity reaches the dashboard.

Access is then **filtered per user** — you see only the resources you have RBAC
read access to:

- The dashboard runs a `SubjectAccessReview` for each node ("can this user `get`
  this Gateway / Route / Service / Pod / IPAddressPool?") and **hides** what you
  can't read.
- **Cluster-admins see everything.**
- **You do not need MetalLB-namespace access** to see the pool/VIP context for
  your own Gateways: a pool is shown whenever it backs a Gateway you can see. In
  that case it is marked **restricted** (🔒) and rendered without a console link,
  so no additional MetalLB detail is exposed. Pools you can read directly are
  shown fully (and empty ones appear too).
- **Log out** clears your session (via the proxy's `/oauth/sign_out`).

Auth can be disabled for local/dev only with `--dashboard-auth-required=false`
(the dashboard then serves everything unauthenticated — do not use in
production).

### Finding the dashboard

Three easy ways, in order of convenience:

1. **Application Launcher** — the operator publishes an OpenShift `ConsoleLink`,
   so **"Beacon Dashboard"** appears under the grid/9-dots menu (top-right of
   every console page), section *Observability*.
2. **Installed Operators** — on the Beacon operator's details page, under
   **Links → Topology Dashboard**.
3. **Directly via the Route**:

   ```bash
   oc -n <beacon-namespace> get route beacon-dashboard \
     -o jsonpath='https://{.spec.host}/{"\n"}'
   ```

Machine-readable status is at `GET <route-host>/api/topology` (also
authenticated and RBAC-filtered).

> **The dashboard's Service, Route, and ConsoleLink are created and owned by the
> operator at startup** — you do not apply them yourself. On OpenShift the Route
> is `reencrypt` (TLS to the oauth-proxy via a service-serving cert). On
> non-OpenShift clusters the Route/ConsoleLink are skipped automatically.

### Endpoints

The manager serves the dashboard on `--dashboard-bind-address` (default
`127.0.0.1:8082` when auth is enabled; reached only through the oauth-proxy on
`:9443`):

| Path              | Description                                   |
| ----------------- | --------------------------------------------- |
| `/`               | The HTML dashboard.                           |
| `/api/topology`   | The topology graph as JSON (RBAC-filtered).   |
| `/healthz`        | Dashboard liveness.                           |
| `/oauth/sign_out` | Log out (provided by the oauth-proxy sidecar).|

### How status is derived

The dashboard reads live cluster objects (pools, gateways, routes, services,
endpointslices, pods, and the Gateway proxy `Deployment`s) through the manager's
cache, then applies the per-user RBAC filter described above. Advertisement
state is derived from **ground truth** — whether the Gateway's proxy
`Deployment` is scaled to zero (`Withdrawn`) or running (`Advertised`) — plus the
policy's shared status for the transient `PendingWithdrawal` /
`PendingReadvertise` states and timer countdowns, so the view is consistent on
every replica and survives controller restarts. Pod health uses the exact same
probe rules as the reconciler (probe-less pods are `Exempt`).

---

## Observability

- **Status dashboard/URL**: the topology dashboard (see above) is the primary
  status surface — `https://<route-host>/` for the UI and
  `https://<route-host>/api/topology` for JSON (both authenticated and
  RBAC-filtered). Find it via the console Application Launcher ("Beacon
  Dashboard") or the operator's Links.
- **Events**: Beacon emits `Withdrawn` (Warning) and `Readvertised` (Normal)
  events on the affected Gateway, including the IP(s) and the elapsed timer.
- **Status**: `GatewayHealthPolicy.status` aggregates counts of managed
  gateways, advertised IPs, and withdrawn IPs, plus a `Ready` condition:
  ```bash
  kubectl get gatewayhealthpolicy cluster
  # NAME      MANAGED   ADVERTISED   WITHDRAWN   PAUSED   AGE
  # cluster   3         2            1           false    29m
  ```
- **Metrics**: the manager serves Prometheus metrics on `:8443` (HTTPS; HTTP/2
  disabled to mitigate Rapid Reset CVE-2023-44487 / CVE-2023-39325). Beacon
  exposes domain metrics in addition to the controller-runtime defaults:

  | Metric | Type | Description |
  | ------ | ---- | ----------- |
  | `beacon_managed_gateways` | gauge | Gateways currently managed. |
  | `beacon_advertised_ips` | gauge | VIPs currently advertised. |
  | `beacon_withdrawn_ips` | gauge | VIPs currently withdrawn. |
  | `beacon_gateway_healthy{gateway_namespace,gateway_name}` | gauge | Per-Gateway health (1/0). |
  | `beacon_gateway_advertised{gateway_namespace,gateway_name}` | gauge | Per-Gateway advertisement (1/0). |
  | `beacon_withdrawals_total{…}` | counter | Route withdrawals performed. |
  | `beacon_readvertisements_total{…}` | counter | Route re-advertisements performed. |
  | `beacon_reconcile_errors_total` | counter | Reconcile errors. |

- **OpenShift monitoring**: the operator provisions a metrics `Service` (with a
  service-serving cert), a `ServiceMonitor`, and a `PrometheusRule` at startup,
  so metrics are scraped by **OpenShift user-workload monitoring** and alerts
  fire out of the box (e.g. `BeaconGatewayVIPWithdrawn`, `BeaconGatewayUnhealthy`,
  `BeaconManyWithdrawnIPs`, `BeaconReconcileErrors`, `BeaconControllerDown`).
  Enable user-workload monitoring on the cluster to consume them:

  ```bash
  oc -n openshift-monitoring get configmap cluster-monitoring-config \
    || oc -n openshift-monitoring create configmap cluster-monitoring-config \
       --from-literal=config.yaml='enableUserWorkload: true'
  ```

  (On non-OpenShift clusters without the Prometheus Operator CRDs, the
  ServiceMonitor/PrometheusRule are skipped automatically.)

---

## Configuration reference

### `GatewayHealthPolicy.spec`

| Field                              | Type       | Default                | Description                                                                 |
| ---------------------------------- | ---------- | ---------------------- | --------------------------------------------------------------------------- |
| `withdrawAfter`                    | duration   | `5s`                   | Continuous-unhealthy time before withdrawing a route.                       |
| `readvertiseAfter`                 | duration   | `30s`                  | Continuous-healthy time before restoring a route.                           |
| `resyncInterval`                   | duration   | `10s`                  | Worst-case reconcile cadence between watch events.                          |
| `minHealthyBackendPercent`         | int (0–100)| `100`                  | Min % of a Gateway's counted backend services that must be healthy to stay advertised (inclusive). 100 = any counted backend down withdraws. See [Multiple backends per Gateway](#multiple-backend-services-per-gateway). |
| `minHealthyPodPercent`             | int (0–100)| `1`                    | Min % of a backend Service's probed pods that must be Ready for that Service to count as healthy (inclusive). 1 = a single Ready pod keeps it up. Overridable per-Gateway and per-Service. |
| `zeroReplicasPolicy`               | `Unhealthy`\|`Exempt` | `Unhealthy` | How to treat a backend Service scaled to zero (selector but 0 pods). `Unhealthy` counts it as a failing backend (can withdraw the VIP); `Exempt` ignores it. Selector-less Services are always exempt. Overridable per-Gateway and per-Service. |
| `paused`                           | bool       | `false`                | Observe-only mode; Beacon takes no withdraw/restore action.                 |
| `gatewayClassNames`                | []string   | `[]` (all)             | Restrict management to these Gateway classes.                              |
| `exemptions`                       | []ref      | `[]`                   | Gateways (namespace/name) to exclude.                                       |
| `metallb.namespace`                | string     | `metallb-system`       | Namespace whose `IPAddressPool`s are read to attribute Gateway VIPs to MetalLB. |

### Annotations (on `Gateway`)

| Annotation                       | Values     | Effect                                       |
| -------------------------------- | ---------- | -------------------------------------------- |
| `beacon.io/exempt`               | `"true"`   | Exempt this Gateway from all management.     |
| `beacon.io/withdraw-after`       | duration   | Override `withdrawAfter` for this Gateway.   |
| `beacon.io/readvertise-after`    | duration   | Override `readvertiseAfter` for this Gateway.|
| `beacon.io/min-healthy-percent`  | int (0–100)| Override `minHealthyBackendPercent` for this Gateway. |
| `beacon.io/min-healthy-pod-percent` | int (0–100)| Override `minHealthyPodPercent`. On a **Gateway**: the default for its Services. On a **backend Service**: overrides the Gateway value for that Service. |
| `beacon.io/zero-replicas-policy` | `Unhealthy`\|`Exempt` | Override `zeroReplicasPolicy`. On a **Gateway**: the default for its Services. On a **backend Service**: overrides the Gateway value for that Service. |

### Manager flags

| Flag                          | Default            | Description                                                        |
| ----------------------------- | ------------------ | ----------------------------------------------------------------- |
| `--policy-name`               | `cluster`          | Name of the singleton `GatewayHealthPolicy` to load config from.  |
| `--dashboard-bind-address`    | `:8082`            | Address the dashboard binds to (set empty to disable the UI).     |
| `--dashboard-auth-required`   | `true`             | Require OpenShift OAuth + per-user RBAC filtering for the dashboard. |
| `--leader-elect`              | `false`†           | Enable leader election (the shipped deployment sets this true).    |
| `--metrics-bind-address`      | `:8443`            | Metrics endpoint (HTTPS; HTTP/2 disabled).                        |
| `--health-probe-bind-address` | `:8081`            | Health/readiness probe endpoint.                                  |

† The deployed manifests/CSV run with `--leader-elect` and
`--dashboard-auth-required=true`; the flag defaults above are the binary's
built-in defaults.

---

## Architecture

```
                       ┌───────────────────────────────────────────┐
                       │              Beacon manager                │
                       │  (controller-runtime, leader-elected HA)   │
                       └───────────────────────────────────────────┘
                                          │ watches
   ┌─────────┬─────────┬─────────┬─────────┬──────────┬──────────────────┐
   ▼         ▼         ▼         ▼         ▼          ▼                  ▼
Gateway   xRoutes   Service  EndpointSlice Pod   (HTTP/GRPC/TCP/TLS)  GatewayHealthPolicy
(gw-api)  (backends)         (discovery)                              (beacon.io)

   For each Gateway:
     trace VIP ──▶ proxy LoadBalancer Service + proxy Deployment
     trace health ──▶ xRoutes ──▶ backend Service(s) ──▶ EndpointSlices
                        ──▶ backend Pods ──▶ probe health
        │
        ├─ infer IPs, match against MetalLB IPAddressPools
        │
        └─ dampening state machine ──▶ scale proxy Deployment 0 / N
                                          (drain / restore endpoints)
                                                     │
                                                     ▼
                                   MetalLB natively withdraws / announces
                                   the /32 (Service has 0 / >0 endpoints)
                                   over the existing BGP session
```

Source layout:

| Path                        | Responsibility                                              |
| --------------------------- | ---------------------------------------------------------- |
| `api/v1alpha1/`             | `GatewayHealthPolicy` API types.                          |
| `internal/trace/`           | Gateway → xRoutes → backend Service → EndpointSlice → backend Pod resolution (VIP from proxy Service). |
| `internal/health/`          | Probe-based health evaluation and exemption rules.        |
| `internal/metallb/`         | Minimal MetalLB CR types + IP/pool matching.              |
| `internal/skupper/`         | Skupper Listener detection + remote-backend health evaluation. |
| `internal/advertiser/`      | Proxy-drain withdraw/restore logic (scale proxy Deployment; BGP-safe). |
| `internal/policy/`          | Exemption, class filtering, timer-override resolution.    |
| `internal/controller/`      | The reconciler and dampening state machine.               |
| `internal/state/`           | Thread-safe store of live advertisement/health state shared with the UI. |
| `internal/topology/`        | Builds the hierarchical, status-annotated topology graph (incl. per-user RBAC filtering). |
| `internal/webui/`           | Dashboard HTTP server, SubjectAccessReview authorizer, and startup provisioning of the dashboard Service/Route/ConsoleLink + oauth-proxy resources. |
| `internal/version/`         | Operator build version (stamped at build time via ldflags). |
| `cmd/main.go`               | Manager entrypoint.                                       |
| `config/`                   | CRDs, RBAC, manager deployment (kustomize).               |
| `bundle/`                   | OLM bundle (CSV + metadata) for OperatorHub.              |

---

## Development

```bash
# Format, vet, and run unit tests.
make test

# Regenerate CRDs / RBAC / DeepCopy (requires controller-gen).
make manifests generate

# Build the binary / image.
make build
make docker-build IMG=ghcr.io/<you>/beacon:dev

# Lint.
make lint
```

Unit tests cover the pure logic (health evaluation, IP/pool matching, policy
resolution). Contributions should keep these deterministic and table-driven.

---

## FAQ / troubleshooting

**Beacon isn't withdrawing an obviously-unhealthy Gateway.**
Check that (a) the Gateway's IP falls within a MetalLB `IPAddressPool`, (b) there
is at least one xRoute (`HTTPRoute`/`GRPCRoute`/`TCPRoute`/`TLSRoute`) whose
`parentRefs` attach it to the Gateway and whose `backendRefs` point at the
failing Service, (c) the **backend** workload pods actually declare probes
(probe-less pods are exempt by design), (d) the Gateway isn't exempt, and (e) its
class isn't filtered out by `gatewayClassNames`.

**Note:** Beacon evaluates the health of the **backend workload pods** reached
through the Gateway's routes — not the Gateway's own proxy pods. A Gateway with
no attached routes (no backends) has nothing to health-check and is treated as
`Exempt`.

**Will withdrawing a route reset my BGP session?**
No. Beacon scales the Gateway's proxy `Deployment` to zero, which drains the
proxy `Service`'s endpoints; MetalLB then withdraws just that `/32` over its
**existing** BGP session. The neighbor stays `Established` and other Gateways'
VIPs are unaffected. (Verified on-cluster.)

**Does Beacon modify my MetalLB pools or advertisements?**
No. Beacon never creates or edits `IPAddressPool` / `BGPAdvertisement` objects.
It only reads pools (to attribute VIPs) and scales the Gateway proxy Deployment
to trigger MetalLB's native withdrawal.

**A whole Gateway's traffic dropped when only one backend was unhealthy.**
Withdrawal is **whole-Gateway** by design (the lever is the shared proxy
Deployment). If a Gateway fronts multiple backends/VIPs and you need per-route
granularity, split them across separate Gateways (one Gateway per VIP).

**A Skupper-linked backend is failing on the remote cluster but the VIP stays up.**
Check that (a) the backend Service carries the `internal.skupper.io/listener`
label (that's how Beacon recognizes it as Skupper-backed), (b) the
`skupper.io/v2alpha1` `Listener` exists in that namespace and its status reflects
the failure (e.g. `status: Pending`, *"No matching connectors"*), and (c) the
operator can read `skupper.io/listeners`. When the Listener is `Ready` again,
Beacon re-advertises after `readvertiseAfter`. See
[Skupper-linked (remote) backends](#skupper-linked-remote-backends).

**Why is a healthy Gateway still showing `PendingReadvertise`?**
It's inside the `readvertiseAfter` dampening window. Once the workload has been
continuously healthy for that duration, the route is restored.

**Can I make it withdraw immediately?**
Set `withdrawAfter: 0s` (globally) or the `beacon.io/withdraw-after: "0s"`
annotation on a specific Gateway. A tiny non-zero value is generally safer.

**Does it work with L2 (ARP/NDP) mode?**
No. Beacon is built specifically for MetalLB in **L3 / BGP mode** and manages BGP
route advertisement only. **L2 mode is explicitly out of scope and there are no
plans to support it.** The operator's design goals — withdrawing an individual
route without disturbing an upstream BGP adjacency, and reasoning about per-VIP
`/32` advertisements — are BGP concepts that do not map onto MetalLB's L2
announcement model, so running Beacon against an L2-mode MetalLB is unsupported.

---

## License

Apache License 2.0. See [LICENSE](./LICENSE).
