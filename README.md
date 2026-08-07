# Beacon

**Beacon** is an OpenShift/Kubernetes operator that couples **Gateway API**
Gateway health to **MetalLB** BGP route advertisements. It continuously watches
your Gateways, traces each one down to the pods that actually serve its traffic,
and — based on those pods' health probes — withdraws or restores the Gateway's
load-balancer IP in MetalLB. All of this happens **without restarting MetalLB or
flapping BGP adjacencies**.

Think of it as a health-aware route dampener that keeps your BGP-advertised
Gateway VIPs pointed only at healthy backends.

---

## Table of contents

- [Why Beacon?](#why-beacon)
- [How it works](#how-it-works)
  - [Tracing a Gateway to its pods](#tracing-a-gateway-to-its-pods)
  - [Health rules](#health-rules)
  - [Withdrawing without flapping BGP](#withdrawing-without-flapping-bgp)
  - [Dampening state machine](#dampening-state-machine)
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
2. **Identifies the proxy `Service`(s)** — the `LoadBalancer` Service the Gateway
   implementation created (via the `gateway.networking.k8s.io/gateway-name`
   label, or by matching ingress IPs). This Service is used **only** as the VIP
   and as the object whose advertisement label Beacon toggles — never as a
   source of health pods.
3. **Determines whether the IP is from MetalLB** by matching it against the
   CIDRs/ranges of every `IPAddressPool` in the MetalLB namespace. IPs that are
   **not** from MetalLB are observed but never mutated.
4. **Discovers the backend `Service`(s)** by listing every xRoute whose
   `parentRefs` attach it to this Gateway and collecting the `backendRefs`
   (core `Service` kind, including cross-namespace refs).
5. **Traces each backend Service to its pods** via `EndpointSlice`s (falling
   back to the Service selector), then inspects each pod's containers' probes.

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

A **Gateway** is then:

- **Unhealthy** if at least one non-exempt (probed) backing pod is failing.
- **Exempt** if every backing pod is exempt, or it has no pods.
- **Healthy** if it has at least one probed pod and none are failing.

> Only a Gateway that is **Unhealthy** and whose IP is **sourced from MetalLB**
> is eligible for withdrawal.

### Withdrawing without flapping BGP

This is the core safety property. MetalLB's `speaker` keeps a **persistent BGP
session** with the upstream router. Which `/32` routes it advertises over that
session is driven by the set of Services selected by `BGPAdvertisement` CRs.

Beacon uses the **`ServiceLabel`** strategy (default):

- Beacon owns a `BGPAdvertisement` (default name `beacon-managed`) whose
  `serviceSelectors` require the label `beacon.io/advertise=true`.
- To **withdraw** a route, Beacon sets `beacon.io/advertise=false` on the
  backing Service. MetalLB sends a single BGP **withdraw** for just that prefix
  over the existing session.
- To **restore** it, Beacon sets `beacon.io/advertise=true`, and MetalLB sends a
  single BGP **announce**.

The BGP session itself is never torn down, so **no adjacency down/up event
occurs** and no other prefixes are disturbed.

> An alternative `DedicatedPool` strategy (moving the IP into a per-IP pool and
> toggling a pool-scoped advertisement) is modeled in the API for environments
> that prefer pool-based control.

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

## Prerequisites

- OpenShift 4.14+ (or Kubernetes 1.27+).
- [Gateway API](https://gateway-api.sigs.k8s.io/) CRDs installed, with at least
  one Gateway implementation provisioning `LoadBalancer` Services.
- [MetalLB](https://metallb.io/) installed in **BGP mode** (the MetalLB Operator
  on OpenShift), with `IPAddressPool` and `BGPPeer` configured.

---

## Installation

### Install via OLM (OperatorHub)

Once published to a catalog, install through the OpenShift console
(**Operators → OperatorHub → Beacon**) or with a `Subscription`:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: beacon
  namespace: openshift-operators
spec:
  channel: stable
  name: beacon
  source: community-operators
  sourceNamespace: openshift-marketplace
```

To build and push the bundle yourself:

```bash
make bundle IMG=quay.io/<you>/beacon:latest VERSION=0.1.0
make bundle-build bundle-push BUNDLE_IMG=quay.io/<you>/beacon-bundle:0.1.0
operator-sdk run bundle quay.io/<you>/beacon-bundle:0.1.0
```

### Install with kustomize

```bash
# Install the CRD.
make install

# Deploy the controller (namespace beacon-system).
make deploy IMG=quay.io/<you>/beacon:latest

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
    namespace: metallb-system
    withdrawalStrategy: ServiceLabel
    advertisementLabelKey: beacon.io/advertise
    managedBGPAdvertisementName: beacon-managed

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

Beacon manages its own `BGPAdvertisement` selecting Services labeled
`beacon.io/advertise=true`. So that Beacon controls advertisement, ensure your
**other** `BGPAdvertisement`s do **not** also select the Gateway Services you
want Beacon to manage. A minimal MetalLB setup:

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
# Beacon creates/owns a BGPAdvertisement named "beacon-managed" automatically.
# You still need a BGPPeer defining the upstream router session:
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

When Beacon first reconciles a MetalLB-backed Gateway, it creates the
`beacon-managed` `BGPAdvertisement` with the appropriate `serviceSelectors`.

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

Beacon serves a built-in, read-only web dashboard that renders the full
relationship chain **from the MetalLB IP all the way down to the app/service
pod**, with live status at every level:

```
IPAddressPool (MetalLB)
  └─ VIP  (Advertised / Withdrawn / Pending)
       └─ Gateway            (health + advertisement)
            └─ Route          (HTTP/GRPC/TCP/TLS, hostnames)
                 └─ Service    (backend)
                      └─ Pod   (phase, probe status, node)
```

Each node is color-coded (Healthy / Degraded / Unhealthy / Withdrawn / Pending /
Exempt / Unknown), the tree is collapsible, and the page auto-refreshes every 5s.
A header summarizes pool/gateway/route/service/pod counts and advertised vs.
withdrawn IPs.

### Endpoints

The dashboard is served by the manager on `--dashboard-bind-address` (default
`:8082`):

| Path             | Description                                  |
| ---------------- | -------------------------------------------- |
| `/`              | The HTML dashboard.                          |
| `/api/topology`  | The full topology graph as JSON.             |
| `/healthz`       | Dashboard liveness.                          |

The JSON API is handy for scripting/alerting, e.g.:

```bash
kubectl -n beacon-system port-forward deploy/beacon-controller-manager 8082:8082
curl -s localhost:8082/api/topology | jq '.summary'
```

### Accessing on OpenShift

The `make deploy` / kustomize config creates a `Service` (`beacon-dashboard`) and
an edge-terminated `Route` (`beacon-dashboard`) in `beacon-system`:

```bash
oc -n beacon-system get route beacon-dashboard -o jsonpath='{.spec.host}{"\n"}'
# open https://<that-host>/ in a browser
```

> The dashboard is **read-only** (it never mutates cluster state), but it does
> expose topology/health information. In production, protect it — e.g. front it
> with the OpenShift OAuth proxy, restrict the Route, or drop the Route and rely
> on `port-forward`. Set `--dashboard-bind-address=""` to disable the UI
> entirely.

### How status is derived

The dashboard combines two sources: (1) live cluster objects (pools, gateways,
routes, services, endpointslices, pods) read through the manager's cache, and
(2) the running controller's in-memory advertisement decisions (Advertised /
Withdrawn / Pending) — which are not otherwise derivable from cluster objects
alone. Pod health uses the exact same probe rules as the reconciler (probe-less
pods are `Exempt`).

---

## Observability

- **Events**: Beacon emits `Withdrawn` (Warning) and `Readvertised` (Normal)
  events on the affected Gateway, including the IP(s) and the elapsed timer.
- **Status**: `GatewayHealthPolicy.status` aggregates counts of managed
  gateways, advertised IPs, and withdrawn IPs, plus a `Ready` condition.
- **Metrics**: the manager serves controller-runtime metrics on `:8443`
  (HTTPS). HTTP/2 is disabled by default to mitigate Rapid Reset (CVE-2023-44487
  / CVE-2023-39325).

---

## Configuration reference

### `GatewayHealthPolicy.spec`

| Field                              | Type       | Default                | Description                                                                 |
| ---------------------------------- | ---------- | ---------------------- | --------------------------------------------------------------------------- |
| `withdrawAfter`                    | duration   | `5s`                   | Continuous-unhealthy time before withdrawing a route.                       |
| `readvertiseAfter`                 | duration   | `30s`                  | Continuous-healthy time before restoring a route.                           |
| `resyncInterval`                   | duration   | `10s`                  | Worst-case reconcile cadence between watch events.                          |
| `paused`                           | bool       | `false`                | Observe-only mode; no MetalLB mutations.                                    |
| `gatewayClassNames`                | []string   | `[]` (all)             | Restrict management to these Gateway classes.                              |
| `exemptions`                       | []ref      | `[]`                   | Gateways (namespace/name) to exclude.                                       |
| `metallb.namespace`                | string     | `metallb-system`       | Namespace of MetalLB CRs.                                                   |
| `metallb.withdrawalStrategy`       | enum       | `ServiceLabel`         | `ServiceLabel` or `DedicatedPool`.                                          |
| `metallb.advertisementLabelKey`    | string     | `beacon.io/advertise`  | Service label key toggled to advertise/withdraw.                            |
| `metallb.managedBGPAdvertisementName` | string  | `beacon-managed`       | Name of the `BGPAdvertisement` Beacon owns.                                |

### Annotations (on `Gateway`)

| Annotation                       | Values     | Effect                                       |
| -------------------------------- | ---------- | -------------------------------------------- |
| `beacon.io/exempt`               | `"true"`   | Exempt this Gateway from all management.     |
| `beacon.io/withdraw-after`       | duration   | Override `withdrawAfter` for this Gateway.   |
| `beacon.io/readvertise-after`    | duration   | Override `readvertiseAfter` for this Gateway.|

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
     trace VIP ──▶ proxy LoadBalancer Service (advertisement target)
     trace health ──▶ xRoutes ──▶ backend Service(s) ──▶ EndpointSlices
                        ──▶ backend Pods ──▶ probe health
        │
        ├─ infer IPs, match against MetalLB IPAddressPools
        │
        └─ dampening state machine ──▶ toggle `beacon.io/advertise` label
                                          on the proxy Service
                                                     │
                                                     ▼
                                        MetalLB BGPAdvertisement
                                        (announce/withdraw a single /32
                                         over the existing BGP session)
```

Source layout:

| Path                        | Responsibility                                              |
| --------------------------- | ---------------------------------------------------------- |
| `api/v1alpha1/`             | `GatewayHealthPolicy` API types.                          |
| `internal/trace/`           | Gateway → xRoutes → backend Service → EndpointSlice → backend Pod resolution (VIP from proxy Service). |
| `internal/health/`          | Probe-based health evaluation and exemption rules.        |
| `internal/metallb/`         | Minimal MetalLB CR types + IP/pool matching.              |
| `internal/advertiser/`      | Advertise/withdraw logic (BGP-safe).                      |
| `internal/policy/`          | Exemption, class filtering, timer-override resolution.    |
| `internal/controller/`      | The reconciler and dampening state machine.               |
| `internal/state/`           | Thread-safe store of live advertisement/health state shared with the UI. |
| `internal/topology/`        | Builds the hierarchical, status-annotated topology graph.  |
| `internal/webui/`           | HTTP server + embedded HTML/CSS/JS topology dashboard.     |
| `cmd/main.go`               | Manager entrypoint.                                       |
| `config/`                   | CRDs, RBAC, manager deployment, dashboard Service/Route (kustomize). |
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
make docker-build IMG=quay.io/<you>/beacon:dev

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
No. Beacon toggles a Service label that changes which prefixes MetalLB
advertises over its **existing** session. Only the affected `/32` is
announced/withdrawn; the neighbor stays established.

**Why is a healthy Gateway still showing `PendingReadvertise`?**
It's inside the `readvertiseAfter` dampening window. Once the workload has been
continuously healthy for that duration, the route is restored.

**Can I make it withdraw immediately?**
Set `withdrawAfter: 0s` (globally) or the `beacon.io/withdraw-after: "0s"`
annotation on a specific Gateway. A tiny non-zero value is generally safer.

**Does it work with L2 (ARP) mode?**
The current release targets BGP mode. L2 advertisement handling is a planned
enhancement.

---

## License

Apache License 2.0. See [LICENSE](./LICENSE).
