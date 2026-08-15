# Beacon health & advertisement states

This document is a reference for **every health/advertisement state Beacon can
show in the topology dashboard**, and a **truth-table matrix** of what inputs
produce which state — evaluated against Beacon's **default policy values**.

If you change any `GatewayHealthPolicy` field (or the per‑Gateway/per‑Service
annotation overrides), re-read [Thresholds](#how-the-thresholds-work) and
recompute; the matrices below assume the defaults.

> Scope: this describes what the UI renders. The controller only ever *decides*
> `Healthy`/`Unhealthy`/`Exempt` and an advertisement state; the extra UI-only
> `Degraded` state is derived in the topology builder.

---

## Table of contents

- [The seven states](#the-seven-states)
- [Two orthogonal axes: health vs advertisement](#two-orthogonal-axes-health-vs-advertisement)
- [Default policy values](#default-policy-values)
- [How the thresholds work](#how-the-thresholds-work)
- [Matrix 1 — Pod (backend leaf)](#matrix-1--pod-backend-leaf)
- [Matrix 2 — backend Service (from its pods)](#matrix-2--backend-service-from-its-pods)
- [Matrix 3 — Gateway health (from its counted backends)](#matrix-3--gateway-health-from-its-counted-backends)
- [Matrix 4 — Gateway advertisement (dampening state machine)](#matrix-4--gateway-advertisement-dampening-state-machine)
- [Matrix 5 — the folded Gateway chip (health + advertisement combined)](#matrix-5--the-folded-gateway-chip-health--advertisement-combined)
- [Matrix 6 — zeroReplicasPolicy (scaled-to-zero backends)](#matrix-6--zeroreplicaspolicy-scaled-to-zero-backends)
- [Matrix 7 — critical backends](#matrix-7--critical-backends)
- [Matrix 8 — exemptions, class filtering, and non-MetalLB VIPs](#matrix-8--exemptions-class-filtering-and-non-metallb-vips)
- [Matrix 9 — Skupper-linked (remote) backends](#matrix-9--skupper-linked-remote-backends)
- [Matrix 10 — missing / unknown data](#matrix-10--missing--unknown-data)
- [Worked end-to-end examples (all defaults)](#worked-end-to-end-examples-all-defaults)

---

## The seven states

Every node in the topology tree (Pool → VIP → Gateway → Route → Service → Pod)
is rendered with exactly one of these normalized states:

| Chip | Color | Meaning |
| --- | --- | --- |
| **Healthy** | green | everything below/at this node is nominal |
| **Degraded** | gold | partially unhealthy — some children failing, but still above the withdraw threshold (still advertised) |
| **Unhealthy** | red | failing |
| **Withdrawn** | purple | MetalLB advertisement withdrawn by Beacon (proxy scaled to 0) |
| **Pending** | blue | a dampening timer is running (counting toward withdraw or re-advertise) |
| **Exempt** | gray | excluded from health checking (annotation, config, or no probes) |
| **Unknown** | light gray | state could not be determined (missing data) |

**Not every state appears at every level.** Leaves are simpler than Gateways:

| Level | States it can show |
| --- | --- |
| **Pod / backend leaf** | Healthy, Unhealthy, Exempt |
| **Service** | Healthy, Degraded, Unhealthy, Exempt, Unknown |
| **Route** | worst of its Services (Healthy / Degraded / Unhealthy / Exempt / Unknown) |
| **Gateway** | Healthy, Degraded, Unhealthy, Withdrawn, Pending, Exempt, Unknown |
| **VIP / Pool** | worst of the Gateways under it, plus Withdrawn/Pending from advertisement |

A **Service chip reflects the same threshold verdict used for the Gateway
decision** (`minHealthyPodPercent`), *not* simply the worst individual pod: a
Service that still meets its threshold but has some pods down is **Degraded**
(up, but partially failing), so it will never show red while the Gateway above it
stays up. This makes each level consistent with its parent.

`Withdrawn` and `Pending` are **only produced at the Gateway/VIP/Pool levels** —
never on a Pod, Service, or Route leaf. `Degraded` can appear on a Service, Route,
Gateway, VIP, or Pool (but never on an individual Pod).

**Worst-child folding.** A parent node inherits the worst state among its
children, using this severity order (highest wins):

```
Unhealthy (5) > Withdrawn (4) > Degraded (3) > Pending (2) > Unknown (1) > Healthy (0) = Exempt (0)
```

Note `Exempt` and `Healthy` are both severity 0, so an exempt child never drags a
parent down.

---

## Two orthogonal axes: health vs advertisement

Beacon tracks **two independent things** for each Gateway:

- **Health** — is the backing workload up? One of `Healthy` / `Unhealthy` /
  `Exempt` / `Unknown` (the controller decides these; the UI adds `Degraded`).
- **Advertisement** — is the VIP being announced? One of `Advertised` /
  `PendingWithdrawal` / `Withdrawn` / `PendingReadvertise`.

Health changes **instantly** when backends change. Advertisement changes **only
after a dampening timer elapses** (`withdrawAfter` / `readvertiseAfter`). That gap
is why a Gateway can read health-Unhealthy while its chip shows `Pending` (still
advertised, counting down) or, later, `Withdrawn`.

The single chip you see on a Gateway is the **folded** result — see
[Matrix 5](#matrix-5--the-folded-gateway-chip-health--advertisement-combined).
Advertisement takes precedence: a Withdrawn/Pending advertisement overrides the
health color; the health color only shows while `Advertised`.

---

## Default policy values

These are the `GatewayHealthPolicy` spec defaults the matrices below assume:

| Field | Default | Effect |
| --- | --- | --- |
| `withdrawAfter` | **`5s`** | continuous unhealthy time before the VIP is withdrawn |
| `readvertiseAfter` | **`30s`** | continuous healthy time before the VIP is restored |
| `resyncInterval` | **`10s`** | worst-case reconcile cadence when no event fires |
| `minHealthyBackendPercent` | **`100`** | % of a Gateway's counted backends that must be healthy to stay advertised |
| `minHealthyPodPercent` | **`1`** | % of a Service's probed pods that must be Ready for the Service to count as healthy |
| `zeroReplicasPolicy` | **`Unhealthy`** | a Service with a selector but zero pods counts as a failing backend |
| `paused` | **`false`** | when true, health is still shown but no VIP is ever mutated |
| `metallb.namespace` | **`metallb-system`** | which namespace's `IPAddressPool`s attribute VIPs |
| policy name (controller flag) | **`cluster`** | name of the singleton `GatewayHealthPolicy` |

With **`minHealthyPodPercent=1`**, a **single Ready probed pod** keeps a Service up.
With **`minHealthyBackendPercent=100`**, **any one counted backend down** takes the
Gateway down. These two defaults drive most of the matrices.

---

## How the thresholds work

Exact comparisons (integer math, truncating division):

**Pod → is it counted / healthy?**
- No readiness/liveness/startup probe on any non-init container → **not counted (Exempt)**. Pods with no probes are ignored entirely.
- Has a probe and `PodReady` condition == `True` → **Healthy**.
- Has a probe and not Ready (including still-starting) → **Unhealthy**. There is no separate "Starting" state.
- Terminating (has a deletion timestamp) → ignored (Exempt).

**Service → healthy?** (only probed pods count)

```
readyProbed = (probed pods) − (unhealthy probed pods)
serviceHealthy = (100 * readyProbed / probed) >= minHealthyPodPercent      # inclusive >=
```

If `probed == 0`: the Service is **not counted** (Exempt) — *unless* it has a
selector, `zeroReplicasPolicy=Unhealthy`, and truly zero pods, in which case it is
**counted and failing** (see [Matrix 6](#matrix-6--zeroreplicaspolicy-scaled-to-zero-backends)).

**Gateway → healthy?** (only counted Services form the denominator)

```
gatewayHealthy% = 100 * healthyCounted / counted                          # 0 when counted==0
gatewayUnhealthy = criticalDown OR (gatewayHealthy% < minHealthyBackendPercent)   # strict <
```

- If `counted == 0` → the Gateway is **Exempt** (nothing to health-check).
- If all counted are healthy (`healthy == counted`) → **Healthy**.
- If some counted are down but the % is still `>= threshold` → **Degraded** (UI) and it stays **Advertised**.
- If the % is `< threshold`, or a critical backend is down → **Unhealthy** (triggers the withdraw timer).

---

## Matrix 1 — Pod (backend leaf)

| Pod has a probe? | `PodReady`==True? | Terminating? | Leaf state |
| --- | --- | --- | --- |
| No | — | — | **Exempt** (ignored) |
| Yes | Yes | No | **Healthy** |
| Yes | No (failing) | No | **Unhealthy** |
| Yes | No (still starting) | No | **Unhealthy** |
| Yes | (any) | Yes | **Exempt** (ignored) |

"Has a probe" = readiness **or** liveness **or** startup probe on any non-init
container.

---

## Matrix 2 — backend Service (from its pods)

Assumes default `minHealthyPodPercent=1` and the Service has a selector.
`P` = probed pods, `R` = Ready probed pods. The Service is **up** (meets the
threshold) when `100*R/P >= 1` (i.e. **at least one** Ready probed pod). The chip
is then **Healthy** if *all* probed pods are Ready, or **Degraded** if it's up but
some pods are down; **Unhealthy** only when it drops below the threshold.

| Probed pods (P) | Ready probed (R) | `100*R/P` | ≥ 1? | Some pods down? | Service state | Counted? |
| --- | --- | --- | --- | --- | --- | --- |
| 0 (no probed pods, has pods without probes) | — | — | — | — | **Exempt** | No |
| 0 (selector, zero pods, policy=Unhealthy) | 0 | — | — | — | **Unhealthy** | Yes (scaled-to-zero) |
| 1 | 1 | 100 | yes | no | **Healthy** | Yes |
| 1 | 0 | 0 | no | yes | **Unhealthy** | Yes |
| 3 | 2 | 66 | yes | yes | **Degraded** | Yes |
| 4 | 1 | 25 | yes | yes | **Degraded** | Yes |
| 4 | 4 | 100 | yes | no | **Healthy** | Yes |
| 4 | 0 | 0 | no | yes | **Unhealthy** | Yes |
| 10 | 1 | 10 | yes | yes | **Degraded** | Yes |

> With the default `minHealthyPodPercent=1`, a Service stays **up** as long as one
> probed pod is Ready. If some (but not all) of its pods are down it shows
> **Degraded** (gold) rather than red — matching the fact that the Gateway above
> it also stays up. It only goes **Unhealthy** (red) when it falls below the
> threshold. Raise `minHealthyPodPercent` (policy, or
> `beacon.io/min-healthy-pod-percent` on the Gateway/Service) to require more.

Higher-threshold example (`minHealthyPodPercent=100`, all pods must be Ready — so
there is no "up but partial" band, hence no Degraded):

| P | R | `100*R/P` | ≥ 100? | Service state |
| --- | --- | --- | --- | --- |
| 4 | 4 | 100 | yes | **Healthy** |
| 4 | 3 | 75 | no | **Unhealthy** |
| 2 | 1 | 50 | no | **Unhealthy** |

---

## Matrix 3 — Gateway health (from its counted backends)

Assumes default `minHealthyBackendPercent=100` and **no critical backends**.
`C` = counted Services, `H` = healthy counted Services. Formula: Unhealthy when
`100*H/C < 100` (i.e. **any** counted backend down).

| Counted (C) | Healthy (H) | `100*H/C` | < 100? | Gateway **health** | Advertised while… |
| --- | --- | --- | --- | --- | --- |
| 0 | 0 | — | — | **Exempt** | always (never withdrawn) |
| 1 | 1 | 100 | no | **Healthy** | yes |
| 1 | 0 | 0 | yes | **Unhealthy** | withdraws after timer |
| 3 | 3 | 100 | no | **Healthy** | yes |
| 3 | 2 | 66 | yes | **Unhealthy** | withdraws after timer |
| 4 | 4 | 100 | no | **Healthy** | yes |
| 4 | 3 | 75 | yes | **Unhealthy** | withdraws after timer |

> At the default `100`, there is **no `Degraded`** — a Gateway is either all-up
> (Healthy) or something-down (Unhealthy). `Degraded` appears only when you
> **lower** `minHealthyBackendPercent` so a partial outage is tolerated.

Lowered-threshold example (`minHealthyBackendPercent=50`):

| C | H | `100*H/C` | < 50? | Gateway **health** |
| --- | --- | --- | --- | --- |
| 4 | 4 | 100 | no | **Healthy** |
| 4 | 3 | 75 | no | **Degraded** (some down, still ≥ 50, stays advertised) |
| 4 | 2 | 50 | no | **Degraded** |
| 4 | 1 | 25 | yes | **Unhealthy** (withdraws after timer) |

(Gateway health is `Degraded` whenever it's not Unhealthy, not Exempt, and not
*all* counted backends are healthy.)

---

## Matrix 4 — Gateway advertisement (dampening state machine)

Health drives advertisement through two timers (defaults `withdrawAfter=5s`,
`readvertiseAfter=30s`). Withdraw fires at `elapsed >= withdrawAfter`;
re-advertise at `elapsed >= readvertiseAfter` (both inclusive).

| Current advertisement | Gateway health now | Time in that health | New advertisement |
| --- | --- | --- | --- |
| Advertised | Healthy / Exempt | — | **Advertised** |
| Advertised | Unhealthy | `< 5s` | **PendingWithdrawal** (timer running) |
| Advertised | Unhealthy | `>= 5s` | **Withdrawn** (proxy scaled to 0; Warning event) |
| PendingWithdrawal | recovered to Healthy | — | **Advertised** (timer cancelled) |
| PendingWithdrawal | still Unhealthy | `>= 5s` | **Withdrawn** |
| Withdrawn | Unhealthy | — | **Withdrawn** (stays down) |
| Withdrawn | Healthy | `< 30s` | **PendingReadvertise** (timer running) |
| Withdrawn | Healthy | `>= 30s` | **Advertised** (proxy scaled back up; Normal event) |
| PendingReadvertise | Unhealthy again | — | **Withdrawn** (immediately, timer cancelled) |
| PendingReadvertise | still Healthy | `>= 30s` | **Advertised** |

Both `PendingWithdrawal` and `PendingReadvertise` render as the single **Pending**
(blue) chip. `Withdrawn` renders purple. The raw advertisement string is also
shown as a small pill on the Gateway row.

**`paused=true`:** the timers and proxy scaling are **skipped** — the Gateway keeps
whatever advertisement it last had, while health is still computed and displayed.
There is no dedicated "Paused" chip.

---

## Matrix 5 — the folded Gateway chip (health + advertisement combined)

The chip you see is derived by taking **advertisement first**, then health:

| Advertisement | Health | Folded chip |
| --- | --- | --- |
| Withdrawn | (any) | **Withdrawn** |
| PendingWithdrawal / PendingReadvertise | (any) | **Pending** |
| Advertised | Unhealthy | **Unhealthy** |
| Advertised | Degraded | **Degraded** |
| Advertised | Healthy | **Healthy** |
| Advertised | Exempt | **Exempt** |
| Advertised | (unset/unknown) | **Unknown** |

So the timeline for a total backend outage (all defaults) is:

```
Healthy ──backends fail──▶ Pending (0–5s) ──5s──▶ Withdrawn
Withdrawn ──backends recover──▶ Pending (0–30s) ──30s──▶ Healthy
```

---

## Matrix 6 — zeroReplicasPolicy (scaled-to-zero backends)

A backend Service that has a **selector but zero pods** (nothing to probe):

| Service has selector? | Pods | Effective `zeroReplicasPolicy` | Service state | Counted toward Gateway? |
| --- | --- | --- | --- | --- |
| Yes | 0 | `Unhealthy` (default) | **Unhealthy** | Yes → can withdraw the VIP |
| Yes | 0 | `Exempt` | **Exempt** | No |
| No (e.g. `ExternalName`, headless w/o selector) | — | (either) | **Exempt** | **No — always** |

The default `Unhealthy` catches accidental scale-to-zero "black holes" (the VIP
would otherwise keep advertising an endpoint-less Service). Selector-less Services
can't be reasoned about and are always exempt.

Precedence for the effective policy: **Service annotation > Gateway annotation >
policy spec > default (`Unhealthy`)**, via `beacon.io/zero-replicas-policy`.

---

## Matrix 7 — critical backends

Mark a backend critical with `beacon.io/critical: "true"` (on a Service, a Route,
or the Gateway). A critical backend **overrides `minHealthyBackendPercent`**:

| Backend is critical? | Backend counted? | Backend healthy? | Effect on Gateway |
| --- | --- | --- | --- |
| No | — | — | normal ratio math (Matrix 3) |
| Yes | Yes | Yes | no effect (it's up) |
| Yes | Yes | No | **Gateway Unhealthy regardless of ratio** (`CriticalDown`) |
| Yes | No (no health signal) | — | **no forced withdrawal** (can't be judged) |

Example at default `minHealthyBackendPercent=100` (already strict) — a 10-backend
Gateway with `minHealthyBackendPercent=50`, one critical backend down and the
other nine up: ratio 90% ≥ 50% would normally be `Degraded`, but the critical
backend forces **Unhealthy**.

Precedence: **Service annotation (including an explicit `false`) > Route
annotation > Gateway-level default.**

---

## Matrix 8 — exemptions, class filtering, and non-MetalLB VIPs

| Condition | Health | Advertisement | Folded chip | Notes |
| --- | --- | --- | --- | --- |
| `beacon.io/exempt: "true"` on the Gateway | **Exempt** | Advertised | **Exempt** | never health-checked, never withdrawn |
| Gateway listed in `spec.exemptions` | **Exempt** | Advertised | **Exempt** | same as annotation |
| `spec.gatewayClassNames` set and Gateway's class **not** in it | **Exempt** | Advertised | **Exempt** | filtered out of management |
| Gateway has **no counted backends** (all probe-less/exempt) | **Exempt** | Advertised | **Exempt** | nothing to check |
| Gateway VIP **not** from a MetalLB `IPAddressPool` | Healthy/Degraded/Unhealthy (still computed) | **Advertised** (never mutated) | reflects health | shown under **"Gateways not sourced from MetalLB"** group (group node itself is **Unknown**) |
| Gateway has no IPs yet | (computed) | **Advertised** | reflects health | not managed until it has a MetalLB VIP |

Beacon only manages VIPs it can attribute to a MetalLB pool. Everything else is
**observed but never withdrawn** — health is displayed for visibility only.

---

## Matrix 9 — Skupper-linked (remote) backends

A Service labeled `internal.skupper.io/listener=<name>` is evaluated from its
Skupper `Listener` status instead of local pods. A Skupper backend is **always
counted** (never exempt like a probe-less local Service):

| Skupper condition | Remote leaf state | Counted? | Healthy? |
| --- | --- | --- | --- |
| Listener CRD not installed | (backend skipped, fail-open) | Yes | **Healthy** (`ready=true`) |
| Listener object not found | **Unhealthy** | Yes | No |
| Listener `Ready` condition True (or `status.status==Ready`) | **Healthy** | Yes | Yes |
| Listener not ready (no matching remote connector) | **Unhealthy** | Yes | No |

The leaf renders as a synthetic `remote: <listener>` node (`Remote (Skupper)`).
Critical/threshold rules then apply exactly as for local backends
([Matrix 3](#matrix-3--gateway-health-from-its-counted-backends),
[Matrix 7](#matrix-7--critical-backends)).

---

## Matrix 10 — missing / unknown data

| Situation | Resulting state |
| --- | --- |
| Service exists but has **0 pods** and is exempt (no selector / policy=Exempt) | Service **Exempt**; if it were the only backend → Gateway **Exempt** |
| Service node with **0 pod children** in the tree | Service **Unknown** |
| Route with **0 Service children** | Route **Unknown** |
| VIP with **0 Gateway children** | VIP **Unknown** |
| Empty MetalLB pool (shown to admins) | Pool **Unknown** |
| "Gateways not sourced from MetalLB" group node | **Unknown** |
| Backend Service referenced by a route but **not found** | omitted (not counted) |
| EndpointSlices missing | falls back to selector-based pod listing; if still zero pods → zeroReplicasPolicy path |
| No `GatewayHealthPolicy` present | controller idles (does nothing); UI uses empty defaults (`metallb-system`, thresholds `100`/`1`) |
| Optional route CRDs (TCPRoute/TLSRoute) not installed | those routes simply don't appear (no error) |

`Unknown` (light gray) always means **"couldn't determine"**, not "bad" — it sits
below `Degraded` in severity so it won't mask a real failure elsewhere in the tree.

---

## Worked end-to-end examples (all defaults)

All examples assume the default policy (`minHealthyPodPercent=1`,
`minHealthyBackendPercent=100`, `withdrawAfter=5s`, `readvertiseAfter=30s`,
`zeroReplicasPolicy=Unhealthy`) and a MetalLB-sourced VIP.

**A. One backend Service, 3 pods, all Ready**
Pods: 3× Healthy → Service `100*3/3=100 ≥ 1` → **Healthy** (counted).
Gateway: `100*1/1=100`, not `< 100` → **Healthy** → **Advertised**.
Chip: **Healthy**.

**B. One backend Service, 3 pods, 1 Ready / 2 failing**
Service: `100*1/3=33 ≥ 1` → **Healthy** (default only needs one Ready pod).
Gateway: 1/1 counted healthy → **Healthy** → **Advertised**.
Chip: **Healthy**. (Raise `minHealthyPodPercent` to flag this.)

**C. One backend Service, all pods failing**
Service: `100*0/3=0`, not `≥ 1` → **Unhealthy** (counted).
Gateway: `100*0/1=0 < 100` → **Unhealthy**.
Advertisement: **Pending** for up to 5s, then **Withdrawn**.
Chip: **Pending** → **Withdrawn**. On recovery: **Pending** (≤30s) → **Healthy**.

**D. Two backend Services, one all-down, one all-up**
Services: one **Healthy**, one **Unhealthy** (both counted).
Gateway: `100*1/2=50 < 100` → **Unhealthy** → withdraws after 5s.
Chip: **Pending** → **Withdrawn**.
(With `minHealthyBackendPercent=50`, 50 is not `< 50` → **Degraded**, stays **Advertised**.)

**E. Backend Service scaled to zero (selector, 0 pods)**
Service: zero pods + selector + default policy → **Unhealthy** (counted).
Gateway: `100*0/1=0 < 100` → **Unhealthy** → withdraws after 5s.
Chip: **Pending** → **Withdrawn**.

**F. Backend pods have no probes**
Pods: no probes → **Exempt** (not counted).
Service: 0 probed pods, has pods → **Exempt** (not counted).
Gateway: `counted==0` → **Exempt**; never withdrawn.
Chip: **Exempt**.

**G. Critical backend down among healthy others (`minHealthyBackendPercent=50`)**
Backends: 4 counted, 3 healthy, the 1 down is `beacon.io/critical: "true"`.
Ratio `75 ≥ 50` would be **Degraded**, but critical-down forces **Unhealthy**.
Chip: **Pending** → **Withdrawn**.

**H. Gateway VIP not from MetalLB**
Health still computed (say **Unhealthy**), but advertisement stays **Advertised**
(never mutated). Shown under "Gateways not sourced from MetalLB". Chip: reflects
health (**Unhealthy**) but the VIP is not withdrawn.
