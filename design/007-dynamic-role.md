# 007 — Dynamic Instance Role, CNPG Pull-Model

**Status:** done
**Milestone:** M5.5

Removes the immutable `--role` flag; each instance runs an in-Pod reconciler that watches its Cluster and self-promotes/self-follows based on `status.targetPrimary`/`currentPrimary`.

**Goal:** Remove the immutable `--role=primary|replica` flag from the instance
Pod command and make an instance's role a *dynamic* function of cluster
lifecycle, exactly like CloudNativePG. The operator stops *pushing*
promote/demote/configure over the control API; instead each instance manager
runs an in-Pod reconciler that *watches its own Cluster* and drives its local
mysqld to match `status.targetPrimary` / `status.currentPrimary`. This fixes the
NOTES.md item: today a switchover leaves stale `--role`/`--source-host` args
baked into the (immutable) Pod, so a Pod restart makes a promoted primary try to
replicate from the old primary (split brain).

## Why (the bug we are fixing)

- Pod args are immutable; `--role`/`--source-host` are frozen at Pod creation.
- M5 switchover/failover change roles *live* via the mTLS control API and pin the
  Pod-template hash so role changes do not recreate Pods. The running role is
  therefore correct, but the **baked args are stale**.
- On a Pod restart, `instance run` re-applies the stale args:
  `EnsureReplicaConfigured` reconfigures a promoted primary to follow the old
  primary (`runner.go` + `manager.go:EnsureReplicaConfigured`). Split brain.

## CNPG reference (what we are mirroring)

From CNPG's instance controller (`internal/management/controller/instance_controller.go`):

- The Pod runs `manager instance run` with **no role flag**.
- The instance manager runs a controller-runtime manager **inside the Pod** that
  watches its own `Cluster` (ServiceAccount RBAC: get/list/watch cluster,
  get/patch cluster/status).
- Role is re-derived each reconcile by comparing `instance.GetPodName()` to the
  Cluster status:
  - `reconcilePrimary` (:1176): if `status.targetPrimary == myName` and I am not
    primary → promote myself, then set `status.currentPrimary = myName`.
  - `reconcileOldPrimary` (:590): if `status.targetPrimary != myName` and I am
    still primary → (PG) fast-shutdown; on restart I come back as a replica.
- Actual role is read from the database (`IsPrimary()` = `pg_is_in_recovery`),
  never from a flag — the persisted DB state is the source of truth.
- The operator only writes `status.targetPrimary`; the Pod performs the role
  change.

MySQL adaptation: MySQL can demote/redirect a replica **live** (super_read_only
+ CHANGE SOURCE + START REPLICA) without a restart, so the former primary does
not need CNPG's shutdown dance — it can demote itself in place. We keep the
self-shutdown only as a fallback if live demotion fails.

## Design

### Responsibility split

- **Operator (policy / cluster-wide view).** Unchanged decisions, new mechanism:
  - Provision Pods/PVCs/Services/secrets/certs (as today). `join` still uses the
    then-current primary for the one-time physical clone.
  - Decide *who* should be primary: bootstrap (`targetPrimary = <cluster>-1`),
    planned switchover (validate target + GTID/RPO, then set `targetPrimary`),
    automatic failover (detect unreachable primary, fence by deleting the Pod,
    pick the GTID-safe candidate, set `targetPrimary`).
  - Candidate selection, RPO (GTID containment), RTO (`maxSwitchoverDelay`),
    divergence detection, and **rw/ro/r role-label routing** stay in the operator
    (it has the cluster-wide view). Labels are patched from observed status.
  - Reads `/status` over mTLS for GTID/role/readiness (kept).
- **Instance manager (mechanism / local).** New in-Pod reconciler:
  - Watches its owning `Cluster`. On each reconcile, compares its pod name to
    `status.targetPrimary`:
    - **I am the target and not yet primary** → wait until caught up to my
      source (retrieved == executed, not lagging), then `Promote` locally and
      patch `status.currentPrimary = me` + `currentPrimaryTimestamp`.
    - **I am not the target** → ensure I am a replica of `status.currentPrimary`:
      `Demote` (super_read_only), `CHANGE SOURCE TO host=<currentPrimary>.svc`,
      `START REPLICA`. No-op when already following the right source.
    - **No target yet / currentPrimary empty** → leave persisted state as-is.
  - Talks only to local mysqld (existing control connection) + the k8s API. No
    mTLS needed for self-management.

### Instance command changes (`internal/cmd/manager/instance/run`)

- **Remove** `--role`, `--source-host`. Role becomes dynamic.
- **Keep** the replication *connection* parameters as static config used to build
  the source when following `currentPrimary`: `--replication-user`,
  `--source-port`, `--source-ssl*`. The source **host** is computed from
  `status.currentPrimary` + the cluster namespace/service domain.
- **Add** `--cluster-name`, `--namespace` (or derive from env/downward API) so the
  in-Pod manager can fetch the right Cluster.
- `instance run` startup: drop the `EnsureReplicaConfigured(staleSource)` call;
  just `EnsureReplicaStarted` (resume persisted replication), then hand role
  control to the in-Pod reconciler.

### Runner / supervisor

- `instance.Run` starts: mysqld supervisor → control connection → **in-Pod
  controller-runtime manager** (cache scoped to the single Cluster + own
  namespace) → control API server (status/healthz/readyz/backup). All share the
  process lifecycle and shut down together on SIGTERM/mysqld-exit.
- New package `pkg/management/mysql/instance/rolereconciler` (or
  `internal/management/controller`) with the InstanceReconciler + unit tests
  using a fake client and a fake local mysqld controller.

### Control API surface

- **Keep:** `GET /status`, `/healthz`, `/readyz`, `GET /cluster/backup`.
- **Remove:** `POST /promote`, `/demote`, `/replica/source` and the operator-side
  `HTTPControlClient.Promote/Demote/ConfigureReplica` + `InstanceControlClient`
  mutation methods. The operator keeps only the status-reading client.

### Operator switchover/failover refactor (migrating M5 logic)

- `reconcilePrimaryChange`: replace demote/promote/configure-replica push calls
  with: validate target (ready replica, GTID containment, maxSwitchoverDelay),
  then set `status.targetPrimary` (+timestamp). Wait (requeue) until
  `status.currentPrimary == target` (set by the target's in-Pod manager), then
  patch role labels and normalize status. Old primary and other replicas
  re-point themselves.
- `reconcileFailover`: unchanged detection/fencing/candidate selection; instead
  of calling `Promote` + `ConfigureReplica`, fence (delete old primary Pod) and
  set `status.targetPrimary = candidate`. The candidate self-promotes; survivors
  self-re-point. Operator patches labels once `currentPrimary` flips.
- `reconcileReplicaSources` (operator push repair) is **removed** — replicas
  repair their own source via the in-Pod reconciler. Divergence detection stays
  (operator surfaces errant former primaries; it will not be re-pointed by its
  own reconciler because the operator marks it diverged — instance must honour a
  `Degraded`/blocked signal; see open decision 2).

### RBAC / ServiceAccount

- New per-cluster (or one shared) ServiceAccount for instance Pods; Pods set
  `serviceAccountName`. Role/RoleBinding (namespaced) granting:
  `clusters` get/list/watch, `clusters/status` get/patch (instance sets
  `currentPrimary`). Add markers/manifests; wire `serviceAccountName` in
  `cluster_pod.go`. The operator creates/owns these per cluster.

## Implementation order

1. Add the instance ServiceAccount + Role/RoleBinding (operator-owned) and set
   `serviceAccountName` on instance Pods.
2. Build the in-Pod InstanceReconciler against a fake client + fake local mysqld
   (promote/demote/follow logic), unit-tested. Pure logic first.
3. Wire a scoped controller-runtime manager into `instance.Run`; add
   `--cluster-name`/`--namespace`; remove `--role`/`--source-host`; keep source
   connection flags.
4. Refactor the operator: drop push mutations, set `targetPrimary` for
   bootstrap/switchover/failover, remove `reconcileReplicaSources` and the
   control-client mutators; keep status reads, labels, divergence, RPO/RTO.
5. Remove `POST /promote|/demote|/replica/source` handlers.
6. Update unit tests (operator no longer records promote/demote; asserts
   `targetPrimary` transitions). Update e2e expectations (switchover/failover via
   `status.targetPrimary`, which the e2e already patches).
7. `make generate manifests`, `make lint-fix`, `make test`, integration, e2e.

## Testing

- Unit: InstanceReconciler — becomes primary when target and behind→caught up;
  becomes replica of currentPrimary when not target; no-op when already correct;
  former primary self-demotes. Operator — switchover/failover now assert
  `targetPrimary` set + label flip on `currentPrimary` change (no push calls).
- Integration: two/three testcontainers driven only by a fake Cluster status to
  exercise self-promote/self-follow where practical.
- E2E: existing switchover + failover specs (they already patch
  `status.targetPrimary`); add a **Pod-restart-after-switchover** check — restart
  the promoted primary and assert it stays primary and does not follow the old
  primary (the original bug).

## Decisions (resolved 2026-06-11)

1. **ServiceAccount granularity:** per-Cluster SA + Role/RoleBinding, operator-
   owned and garbage-collected with the Cluster, scoped to watch only that
   Cluster. Least privilege.
2. **Diverged former primary:** the in-Pod reconciler reads cluster status and
   **skips self-follow** when its own name is listed in the diverged set, so the
   operator's loud block is not raced/undone.
3. **currentPrimary writer:** only the promoting instance writes
   `currentPrimary` (+ timestamp). The operator stops writing it from `observe`
   (it only reads it); it may still clear it on teardown.
4. **Self-shutdown fallback:** yes. Try live demote first; if it fails, request
   mysqld shutdown so the Pod restarts clean and returns as a replica.

## Acceptance criteria

- Instance Pods carry no `--role`/`--source-host`; role is derived from cluster
  status at runtime.
- Planned switchover and automatic failover work by the operator setting only
  `status.targetPrimary`; instances self-promote / self-follow.
- Restarting the promoted primary keeps it primary (no follow-the-old-primary
  regression); restarting any replica resumes following the current primary.
- RPO (GTID containment), RTO (`maxSwitchoverDelay`), fencing, divergence
  detection, and rw/ro/r routing still hold.
- `make generate manifests`, `make lint`, `make test`, integration, and the M5.5
  e2e (including Pod-restart) pass.
```
