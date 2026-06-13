---
title: "Operations Runbooks"
description: "Common CNMySQL operational tasks: scaling, switchovers, failover, PVC handling, and status inspection."
sidebar_position: 5
---

# Operations runbooks

This page collects day-two operations for CNMySQL clusters. Commands use
`cluster-sample` as the Cluster name.

## Inspect cluster state

```bash
kubectl get cluster cluster-sample
kubectl describe cluster cluster-sample
kubectl get pods -l mysql.cloudnative-mysql.io/cluster=cluster-sample --show-labels
kubectl get events --field-selector involvedObject.name=cluster-sample
```

Useful status fields:

- `status.readyInstances`
- `status.currentPrimary`
- `status.targetPrimary`
- `status.gtidExecutedByInstance`
- `status.divergedInstances`
- `status.continuousArchiving`
- `status.phase` and `status.phaseReason`

## Scale up

```bash
kubectl patch cluster cluster-sample --type merge -p '{"spec":{"instances":4}}'
kubectl wait --for=condition=Ready cluster/cluster-sample --timeout=15m
```

Scale-up is ordered. CNMySQL creates one replica at a time and waits for it to
be healthy before creating the next one.

## Scale down

```bash
kubectl patch cluster cluster-sample --type merge -p '{"spec":{"instances":1}}'
```

Scale-down removes highest-ordinal replicas first. CNMySQL deletes replica Pods
but retains PVCs. It never scales below one instance and does not remove the
current primary during normal scale-down.

List retained PVCs:

```bash
kubectl get pvc -l mysql.cloudnative-mysql.io/cluster=cluster-sample
```

Delete retained PVCs only after confirming the data is no longer needed.

## Planned switchover

CNMySQL follows the CNPG-style status transition model. A planned switchover is
requested by setting `status.targetPrimary` to a healthy replica.

Example:

```bash
kubectl patch cluster cluster-sample --subresource=status --type merge \
  -p '{"status":{"targetPrimary":"cluster-sample-2"}}'
```

Then watch:

```bash
kubectl get cluster cluster-sample -w
kubectl get pods -l mysql.cloudnative-mysql.io/cluster=cluster-sample --show-labels
```

The operator validates the target, waits for GTID containment, bounds the
operation by `spec.maxSwitchoverDelay`, and lets the selected instance promote
itself. Role Services move after the database role is safe.

## Automatic failover

Automatic failover is driven by primary health, Pod readiness, and GTID safety.
`spec.failoverDelay` controls how long CNMySQL waits after detecting the
primary as failed. `0` means immediate failover.

```yaml
spec:
  failoverDelay: 30
```

During failover CNMySQL:

1. chooses a ready replica with healthy replication SQL state;
2. checks that candidate GTID sets are comparable;
3. fences the old primary Pod while retaining its PVC;
4. sets `targetPrimary` to the safe candidate;
5. updates role labels and Services after promotion.

If GTID sets are divergent or no safe candidate exists, failover is blocked
instead of risking data loss.

## Former primary rejoin

A former primary that returns after failover starts read-only and follows the
current primary if its GTID set is compatible.

If it contains errant transactions, CNMySQL marks it diverged and keeps it out
of service. Do not delete the retained PVC until you have decided whether manual
recovery is required.

Check:

```bash
kubectl get cluster cluster-sample -o jsonpath='{.status.divergedInstances}'
```

## Restart an instance Pod

Deleting a Pod lets the operator recreate it against the retained PVC:

```bash
kubectl delete pod cluster-sample-2
```

Every instance boots read-only. The in-pod role reconciler observes Cluster
status and only clears read-only mode when the instance is the confirmed
primary.

## Change MySQL parameters

Update `spec.mysql.parameters`:

```bash
kubectl patch cluster cluster-sample --type merge -p \
  '{"spec":{"mysql":{"parameters":{"require_secure_transport":"ON"}}}}'
```

CNMySQL owns replication, backup, PITR, identity, and lifecycle-critical
settings. User parameters that conflict with managed keys are rejected by the
configuration layer.

## Backup and restore operations

Create a one-shot Backup for an on-demand recovery point. Use ScheduledBackup
for recurring backup creation.

Deleting the `Backup` Kubernetes object does not delete the remote object-store
artifacts today. Remote cleanup is a planned finalizer/retention feature.

## Continuous archiving operations

When continuous archiving is enabled, inspect:

```bash
kubectl get cluster cluster-sample -o jsonpath='{.status.continuousArchiving}'
```

Growing pending files or a degraded condition usually means an object-store,
credential, network, or throughput issue.

## Safe maintenance habits

- Prefer planned switchover before node or primary maintenance.
- Keep at least three instances for meaningful automatic failover.
- Use semi-sync when acknowledged-write durability matters.
- Keep object-store lifecycle rules aligned with backup and PITR retention.
- Treat retained PVCs and remote backups as recovery assets.
