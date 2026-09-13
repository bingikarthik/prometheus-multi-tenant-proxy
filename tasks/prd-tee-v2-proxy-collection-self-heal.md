# PRD: TEE-V2 prometheus-multi-tenant-proxy — runtime collection reconcile + silent-stall self-heal & alert

**Jira:** ETHOS-115242 (Bug, Major) — *"[TEE-V2] prometheus-multi-tenant-proxy silently stops collecting for a tenant after backend pod churn and does not self-recover (emits up=0, no metrics)"*
**Related:** ETHOS-104793, ETHOS-101083, ETHOS-83987 (TEE-V2 multi-tenant-proxy)
**Component:** IE - Ethos Cluster Observability
**Repo:** `adobe/prometheus-multi-tenant-proxy` (Go)

---

## 1. Introduction / Overview

`prometheus-multi-tenant-proxy` (TEE-V2) collects a tenant's `container_*` / `kubelet_*` / `node_*` metrics from source Prometheus shards and remote-writes them into the tenant's Prometheus. In production it **silently stops collecting for a tenant and never self-recovers**: it keeps emitting the synthetic skeleton (`up{metrics_path!=""}=0`, plus `count:up0`, `ALERTS`, `ALERTS_FOR_STATE`) while real collection returns nothing. The only remediation today is a manual `kubectl -n monitoring rollout restart deploy/prometheus-multi-tenant-proxy`. There is no alert, so the gap is silent for weeks.

**Confirmed root cause (grounded live on `ethos11-prod-sgp2`, 2026-09-11):** the running image `0.0.8` initializes per-tenant remote-write **collection workers only once, at proxy startup** (`Controller.Start → loadExistingRemoteWriteJobs`). A `MetricAccess` CR created **after** the pods start is registered on the *query* path (the tenant manager's 30s refresh loop) but **never gets a collection worker**, so it emits the `up=0` skeleton indefinitely with no data flowing. Evidence: proxy pods 128 days old (~May 6); tenant CR `app-ns-team-mail-relay` created 2026-09-09; running pod logs only `Updated tenant` / `Loaded 1 tenants` and **none** of the collection-path log lines (`Created remote write job`, `Stored collected metrics from all targets`, `Successfully sent metrics via remote write`).

**Key finding — HEAD already contains much of the fix, unreleased.** The current `main` source (this repo) adds:
- `Controller.reconcileLoop` (30s ticker) → `reconcileRemoteWriteJobs`, which lists `MetricAccess` CRs and **creates/stops per-tenant collection jobs at runtime** (addresses reconcile-on-add).
- `createRemoteWriteJobLocked` resolves targets **dynamically each collection cycle** ("get them fresh each time") — addresses the original backend-pod-churn case.
- `discovery/kubernetes.go` re-resolves targets on a refresh ticker.

Therefore this PRD is **verification-first**: reproduce the wedge against the HEAD image, confirm which failure modes HEAD already resolves, and then harden only what remains — plus add the silent-stall **self-heal watchdog** and the **alert** that are still missing regardless of the reconcile fix. The residual silent-failure surface is real: `collectMetrics` stores an empty result and returns **with no error** when `healthyTargets == 0` (or when a target query returns zero series), so a job can be "running" yet forever emit nothing.

## 2. Goals

- A tenant onboarded (or whose backend churns) **after** the proxy starts begins getting collected **without** a manual restart.
- The proxy **detects** a per-tenant collection stall (enabled `MetricAccess`, source data present, but zero series collected/sent for > grace period) and **self-heals** (recreate the in-process job; restart the pod only as a last resort).
- A **fleet-wide alert** fires when an onboarded tenant's collection is silently stalled, so the failure is never again invisible for weeks.
- All behavior is covered by automated tests reproducing the runtime-add and churn scenarios.

## 3. User Stories

### US-001: Reproduce and characterize the wedge against the HEAD image
**Description:** As an engineer, I want a repeatable local/CI reproduction of the silent-stall so I can confirm which failure modes HEAD already fixes and scope the remaining hardening (per decision 2C — verify first).

**Acceptance Criteria:**
- [ ] A test harness (envtest / kind / fake `client.Client`) starts the controller, then creates a `MetricAccess` CR (`remoteWrite.enabled=true`) **after** `Start()` has run.
- [ ] Documented result: whether HEAD's `reconcileLoop` creates the collection job for the runtime-added CR (expected: yes) — captured as a passing/failing assertion.
- [ ] A second scenario changes the backend target set (pod-IP churn) after job creation and asserts whether `collectMetrics` recovers via dynamic target resolution.
- [ ] A short findings note (in this PRD's "Open Questions" or a linked comment) lists any residual gap where a job is "running" but collects zero series with no error.
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` pass.

### US-002: Guarantee runtime reconcile-on-add of remote-write collection jobs (harden)
**Description:** As a tenant, I want my metrics collected within one reconcile interval of my `MetricAccess` CR being created, even though the proxy was already running.

**Acceptance Criteria:**
- [ ] With the controller already started, creating a `MetricAccess` (`remoteWrite.enabled=true`) results in a collection job within ≤ one reconcile interval (≤ 30s).
- [ ] Enabling `remoteWrite` on an existing CR, or changing its metric set, is reconciled (job created / restarted via `hasConfigurationChanged`).
- [ ] Deleting/disabling the CR stops and removes the job (no leaked goroutines — verified via `StopCh` close).
- [ ] Regression test covers create-after-start, enable-after-start, and delete.
- [ ] `go build`/`go vet`/`go test` pass.

### US-003: Detect a silent per-tenant collection stall
**Description:** As an operator, I want the proxy to know when a tenant with an enabled CR is producing zero collected series so it can act and be alerted on.

**Acceptance Criteria:**
- [ ] Per job, track `lastSuccessfulCollectionTime` and `lastCollectedSeriesCount` (a "success" = ≥1 series stored AND remote-write send returned no error).
- [ ] A job is classified **stalled** when: CR is enabled, AND no successful collection for `> stallGracePeriod` (default 15m, configurable), AND (where cheaply checkable) source targets are discoverable.
- [ ] The empty-result branch in `collectMetrics` (`healthyTargets == 0` or zero series) is treated as **not** a success and increments a stall counter rather than silently returning.
- [ ] Unit test drives a job to the stalled state and asserts detection.
- [ ] `go build`/`go vet`/`go test` pass.

### US-004: Self-heal step 1 — auto-recreate the in-process collection job
**Description:** As an operator, I want a stalled collection job to be rebuilt in-process automatically before any pod restart (decision 4D, step B).

**Acceptance Criteria:**
- [ ] When a job is stalled for `> stallGracePeriod`, the watchdog stops it (`stopRemoteWriteJobLocked`) and recreates it (`createRemoteWriteJobLocked`), re-resolving targets fresh.
- [ ] Recreation is rate-limited (e.g. at most once per `recreateBackoff`, default 5m) to avoid churn/thrash.
- [ ] A recovery (next successful collection) resets the stall state and counters.
- [ ] Each recreate is logged with tenant key, stall duration, and attempt count.
- [ ] Test: a job stuck at zero series is auto-recreated and, once targets return data, recovers without manual action.
- [ ] `go build`/`go vet`/`go test` pass.

### US-005: Self-heal step 2 — pod restart as last resort
**Description:** As an operator, I want the pod to restart itself only if in-process recovery fails, so a truly wedged process still recovers unattended (decision 4D, step C).

**Acceptance Criteria:**
- [ ] If a job remains stalled for `> podRestartGracePeriod` (default 3× stallGracePeriod, e.g. 45m) despite ≥1 recreate attempt, the proxy triggers its own restart (process exit non-zero so Kubernetes restarts the container).
- [ ] Restart is guarded: only the remote-write **leader** may self-restart; the escalation is disabled when `--self-heal-pod-restart=false` (default configurable, ships **enabled**).
- [ ] Escalation is throttled (at most once per `podRestartCooldown`, default 30m) and emits a clear WARN log + increments a restart-counter metric before exiting.
- [ ] Leadership failover after restart is verified not to duplicate writes (existing leader-election lease honored).
- [ ] Test covers: escalate-only-after-recreate-failed, leader-only, throttle.
- [ ] `go build`/`go vet`/`go test` pass.

### US-006: Expose per-tenant collection-health metrics
**Description:** As an operator, I want first-class proxy metrics for collection health so alerting does not depend on interpreting the ambiguous `up` skeleton.

**Acceptance Criteria:**
- [ ] Proxy exposes (Prometheus format on its metrics endpoint), labeled by tenant namespace/name:
  - `proxy_tenant_collection_up` (1 = last cycle collected & sent ≥1 series, else 0)
  - `proxy_tenant_last_successful_collection_timestamp_seconds`
  - `proxy_tenant_collected_series` (gauge of last cycle's series count)
  - `proxy_tenant_stall_recreations_total`, `proxy_tenant_self_restart_total` (counters)
- [ ] Metrics appear only for tenants with an enabled `MetricAccess` (no cardinality for absent tenants).
- [ ] Test asserts the gauges flip correctly across stall → recover.
- [ ] `go build`/`go vet`/`go test` pass.

### US-007: Ship a fleet-wide alert on persistent silent stall
**Description:** As an operator, I want a `PrometheusRule` shipped with the proxy that alerts when an onboarded tenant is silently stalled, so a stall is caught within minutes (decision 3A).

**Acceptance Criteria:**
- [ ] A `PrometheusRule` is added to the `ethos-core-prometheus-multi-tenant-proxy` Helm chart (this artifact is authored here; the chart change lands in that repo).
- [ ] Primary expression uses the proxy health metric, e.g. `max by (namespace,name) (proxy_tenant_collection_up) == 0` for `> 15m` → severity warning/major.
- [ ] Corroborating expression documented: `up{metrics_path!=""} == 0` persisting per tenant for `> 15m` (fallback where the health metric is unavailable, e.g. old images mid-rollout).
- [ ] Alert has a runbook annotation pointing at the workaround (`rollout restart`) and this ticket.
- [ ] `promtool test rules` (or chart lint / helm template) passes for the new rule.

### US-008: Tests for runtime-add, churn, and self-heal end-to-end
**Description:** As a maintainer, I want the regression suite to lock in all three failure modes so this cannot silently regress.

**Acceptance Criteria:**
- [ ] Table-driven tests: (a) CR created after start → collected; (b) backend target set changes → recovers; (c) forced zero-collection → watchdog recreate → recover; (d) persistent stall → last-resort restart path invoked (mocked exit).
- [ ] No goroutine leaks across job create/stop (verified with a leak check or explicit `StopCh` assertions).
- [ ] `go test ./...` green in CI.

## 4. Functional Requirements

- **FR-1:** The remote-write controller MUST create/update/stop per-tenant collection jobs in response to `MetricAccess` create/update/delete events at runtime, not only at process start.
- **FR-2:** A `MetricAccess` with `spec.remoteWrite.enabled=true` created after the proxy has started MUST have a running collection job within ≤ one reconcile interval (≤ 30s).
- **FR-3:** `collectMetrics` MUST NOT treat an empty result (`healthyTargets == 0`, or zero series returned) as a success; it MUST record it as a failed/empty cycle for stall detection.
- **FR-4:** The controller MUST track, per job, the last successful collection timestamp and last series count, where "successful" = ≥1 series stored and remote-write send returned no error.
- **FR-5:** The controller MUST classify a job as *stalled* when its CR is enabled and it has had no successful collection for `> stallGracePeriod` (default 15m, configurable).
- **FR-6:** On stall, the controller MUST first recreate the in-process job (stop + recreate with fresh target resolution), rate-limited by `recreateBackoff` (default 5m).
- **FR-7:** If a job remains stalled `> podRestartGracePeriod` (default 45m) despite ≥1 recreate attempt, and self-restart is enabled, the **leader** replica MUST restart itself (non-zero process exit), throttled by `podRestartCooldown` (default 30m).
- **FR-8:** All self-heal thresholds (`stallGracePeriod`, `recreateBackoff`, `podRestartGracePeriod`, `podRestartCooldown`, `self-heal-pod-restart` on/off) MUST be configurable via flags/config with the documented safe defaults; pod-restart escalation ships **enabled**.
- **FR-9:** The proxy MUST expose per-tenant collection-health metrics: `proxy_tenant_collection_up`, `proxy_tenant_last_successful_collection_timestamp_seconds`, `proxy_tenant_collected_series`, `proxy_tenant_stall_recreations_total`, `proxy_tenant_self_restart_total`.
- **FR-10:** A `PrometheusRule` MUST ship in the `ethos-core-prometheus-multi-tenant-proxy` chart alerting on `proxy_tenant_collection_up == 0` for `> 15m` per tenant, with a runbook annotation.
- **FR-11:** Self-heal actions and alert conditions MUST be logged (structured) with tenant key, stall duration, and action taken.
- **FR-12:** Job stop paths MUST close `StopCh` and remove the job from the map with no goroutine leak.

## 5. Non-Goals (Out of Scope)

- **Fleet rollout of the fixed image** (build/version bump, ArgoCD promotion, per-cluster rollout) — tracked separately by the reporter; this PRD delivers the proxy-side code + shipped alert only (decision 1A).
- **The immediate operational workaround** — running `rollout restart` on `ethos11-prod-sgp2` today is an ops action, not part of this deliverable.
- **Tenant-side changes** (eNM config, MetricAccess schema, tenant Prometheus image/config) — the tenant config is already correct in all recurrences.
- **The `tee-metrics-rbac-proxy-client` Role / `metrics.k8s.io` resource-metrics API** — explicitly a red herring per the ticket; v2 cadvisor federation works without it. Do not touch.
- **Rewriting service discovery or the query/load-balancer path** beyond what stall recovery requires.
- **Switching the tenant manager / controller from periodic list to a full watch/informer** — allowed as an optional improvement but not required for correctness here.

## 6. Design Considerations

- Reuse existing structures: `Controller.jobs`, `RemoteWriteJob` (add `lastSuccessAt`, `lastSeriesCount`, `recreateCount`, `lastRecreateAt`), `reconcileRemoteWriteJobs`, `collectMetrics`, `updateJobStatus`.
- The watchdog can run inside the existing `reconcileLoop` (evaluate stall state each 30s tick) rather than adding a new goroutine, keeping all job-map access under the existing `c.mu`.
- Keep the ambiguous `up` skeleton as-is for backward compatibility; the new `proxy_tenant_collection_up` metric is the authoritative health signal for alerting, with `up{metrics_path!=""}==0` documented as the fallback during mixed-version rollout.

## 7. Technical Considerations

- **Leader election:** only the remote-write leader runs collection (`main.go` starts `Controller.Start` inside `OnStartedLeading`). Self-restart (US-005/FR-7) must be leader-only and must not break the lease-based no-duplicate-write guarantee; on restart, `ReleaseOnCancel:true` already enables fast failover.
- **Silent-empty surface:** the concrete residual bug even with reconcile is `collectMetrics` returning success-shaped behavior on empty collection — FR-3/FR-4 are the crux of "self-heal even when reconcile alone isn't enough."
- **Source-data awareness:** true stall detection ideally distinguishes "no data at source" (legitimately empty) from "data present but not collected." Where a cheap check exists (targets discoverable / non-empty), require it before escalating to pod restart to avoid restart loops on genuinely idle tenants.
- **Rate limiting / thrash:** all recovery actions must be backoff-throttled (FR-6/FR-7) so a persistently unhealthy backend cannot cause a restart storm.
- **Version skew during rollout:** old `0.0.8` pods won't emit the new health metric; the alert's fallback (`up{metrics_path!=""}==0`) covers them until the fleet is upgraded.
- **Log anchors** (useful for tests and ops): working collection logs `Stored collected metrics from all targets` / `Sending metrics to Prometheus` / `Successfully sent metrics via remote write`; a wedged tenant logs only `Updated tenant` / `Loaded N tenants`.

## 8. Success Metrics

- Time-to-recovery for a stalled tenant drops from **manual/unbounded (weeks)** to **automatic ≤ 45m** (recreate ≤15m, pod-restart escalation ≤45m) with **zero human action**.
- A tenant onboarded after proxy start is collected within **≤ 30s** (one reconcile interval) with no restart.
- **100%** of silent stalls raise the new alert within **15m** (validated in test/stage).
- **Zero** duplicate-write regressions under leader failover triggered by self-restart.
- Regression suite reproduces all three historical failure modes and stays green in CI.

## 9. Open Questions

- Does the HEAD `reconcileLoop` fully resolve the runtime-add case in a live repro, or does the `collectMetrics` empty-success path still let a job "run" while emitting nothing? (Resolved by US-001; scopes how much of US-002 is net-new vs. verification.)
- Preferred last-resort restart mechanism: process `os.Exit(1)` vs. flipping readiness to force a controller-driven restart — which is cleaner given the 4-container pod and leader lease?
- Stall detection for legitimately idle tenants: is a source-side "targets discoverable" check sufficient to avoid restart loops, or do we need a source-has-data probe before escalation?
- Exact default thresholds (15m / 5m / 45m / 30m) — confirm against SRE alert-fatigue tolerances before shipping the `PrometheusRule`.
- Verification environment (was Q5, deferred by reporter): stage `ethos11-stage-or1` vs. the recurring prod `ethos11-prod-sgp2` vs. a canary set — to be decided before sign-off.
```
