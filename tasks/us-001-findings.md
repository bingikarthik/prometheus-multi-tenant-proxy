# US-001 Findings: Reproduce & characterize the collection wedge vs HEAD

Ticket: ETHOS-115242 · Story: US-001 · Branch: `ralph/tee-v2-proxy-collection-self-heal`

## What was tested

Hermetic, in-package Go tests in `internal/remote_write/controller_selfheal_test.go`
using a controller-runtime **fake** `client.Client` and a **fake**
`discovery.Discovery`. No live cluster, no envtest. The 30s reconcile ticker is
**not** slept on — the test drives `Controller.reconcileRemoteWriteJobs(ctx)`
directly (the test is in `package remote_write`), so the whole suite runs in
milliseconds.

No production seam was required. `reconcileRemoteWriteJobs` and `collectMetrics`
are already unexported methods reachable from an in-package test, which is
exactly the "call the reconcile function directly" option in the acceptance
criteria. Production behavior and defaults are unchanged.

## (a) Runtime-add — FIXED on HEAD

Test: `TestController_RuntimeAddCR_PickedUpByReconcile`

- `Controller.Start` → `loadExistingRemoteWriteJobs` enumerates tenants **once**.
  A MetricAccess CR created *after* `Start()` is therefore **not** picked up
  by the startup path — the test asserts `GetActiveJobs()` is still empty
  immediately after the runtime `client.Create`. This reproduces the shipped
  0.0.8 bug's root cause.
- HEAD adds `reconcileLoop` (30s) → `reconcileRemoteWriteJobs`, whose
  create-if-not-exists branch **does** create a collection job for the
  runtime-added CR. After one direct reconcile call, the job
  `tenant-a/ma-runtime` exists.

Conclusion: **HEAD fixes the runtime-add case**, but only on the next reconcile
tick — worst-case ~30s latency between CR creation and a collection worker
starting. Acceptable, but not instantaneous (there is no watch/event trigger,
only the periodic poll).

## (b) Backend pod-IP churn — FIXED on HEAD

Test: `TestController_ChurnTargets_DynamicResolution`

- `collectMetrics` calls `serviceDiscovery.GetTargets()` **fresh on every run**
  (no targets are cached on the job at creation time; `createRemoteWriteJobLocked`
  deliberately stores no static targets).
- First collection queries server A. After the fake discovery target set is
  mutated (old pod-IP removed, new pod-IP added), the next collection queries
  server B and no longer touches server A.

Conclusion: **HEAD fixes the churn case** — dynamic target resolution works; a
running job follows backend pod-IP changes without a restart.

## (c) RESIDUAL GAP — silent zero-series stall with no error

Test: `TestCollectMetrics_NoHealthyTargets_SilentZeroSeriesNoError`

Even with (a) and (b) fixed, the `healthyTargets == 0` branch in
`collectMetrics` (controller.go ~L444-465) is the remaining wedge:

- When discovery returns **zero healthy targets** (all targets unhealthy, or
  discovery itself is stale/empty), `collectMetrics` stores an **empty** metric
  slice for the tenant, sets `LastRun = now`, and returns.
- Crucially it records **no error** (`job.LastError == nil`), increments **no**
  `FailureCount`, and `updateJobStatus` still marks the job `Active: true`.
- `sendMetrics` is never called (nothing to send), so nothing surfaces the
  emptiness downstream.

Externally the job looks healthy and "running" while producing nothing — this is
the production symptom: the synthetic `up{metrics_path!=""}=0` skeleton keeps
being emitted while real collection returns zero series, and **nothing
self-heals it** (no target-count threshold, no discovery restart, no error
escalation). A job that has silently gone to zero targets is indistinguishable,
from status alone, from a healthy job.

## Recommended follow-up (out of scope for US-001)

Detect and act on the sustained `healthyTargets == 0` condition — e.g. record a
distinct error/condition on the job, expose a per-tenant "targets resolved"
gauge, and/or force a discovery refresh so a wedged tenant can self-recover
without a pod restart.

## Verification

`go build ./...`, `go vet ./...`, `go test ./...`, and
`go test -race ./internal/remote_write/` all pass. The only dependency change is
a `go.sum`/`go.mod` entry for `github.com/evanphx/json-patch` pulled in
transitively by the controller-runtime fake client (test-only).
