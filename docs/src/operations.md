---
title: "Operations Runbooks"
description: "Common CNMySQL operational tasks with the kubectl cnmysql plugin: status, scaling, switchovers, failover, restart, backup, user and database management."
sidebar_position: 8
---

# Operations runbooks

CNMySQL ships a kubectl plugin, `kubectl-cnmysql`, that wraps common day-two
operations. Install it once:

```bash
make install-plugin
```

Most commands accept an optional `CLUSTER` argument. When you omit it, the
plugin picks the only cluster in the current namespace and warns if there are
several.

Commands in this guide use `cluster-sample` as the Cluster name.

## Inspect cluster state

```bash
kubectl cnmysql status
kubectl cnmysql status cluster-sample
```

Add `-w` or `--watch` to refresh every 2s, like watch(1):

```bash
kubectl cnmysql status -w
kubectl cnmysql status -w --watch-interval=5s
```

The status command shows instance topology, phase, conditions, and health. For
raw Kubernetes output, `kubectl describe cluster` and `kubectl get events` still
work and give you more detail when you need it.

Key status fields on the Cluster resource:

- `status.readyInstances`
- `status.currentPrimary`
- `status.targetPrimary`
- `status.gtidExecutedByInstance`
- `status.divergedInstances`
- `status.continuousArchiving`
- `status.phase` and `status.phaseReason`

## Stream logs

```bash
kubectl cnmysql logs cluster-sample          # all instances, merged with a prefix
kubectl cnmysql logs cluster-sample cluster-sample-2  # single instance
```

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

CNMySQL follows the CNPG-style status transition model. A planned switchover
promotes a named healthy replica. Use the plugin:

```bash
kubectl cnmysql promote cluster-sample cluster-sample-2
```

Watch progress:

```bash
kubectl cnmysql status -w
```

The operator validates the target, waits for GTID containment, bounds the
operation by `spec.maxSwitchoverDelay`, and lets the selected instance promote
itself. Role Services move after the database role is safe.

You can also trigger a switchover manually through the subresource:

```bash
kubectl patch cluster cluster-sample --subresource=status --type merge \
  -p '{"status":{"targetPrimary":"cluster-sample-2"}}'
```

## Fence an instance

Fencing takes an instance out of service without deleting it or its data. The
Pod stays, the PVC stays, but the instance drops out of all routing Services and
is held read only:

```bash
kubectl cnmysql fence on cluster-sample cluster-sample-2
```

Unfence it to restore normal routing and role reconciliation:

```bash
kubectl cnmysql fence off cluster-sample cluster-sample-2
```

The operator tracks fenced instances in `status.fencedInstances`. A fenced
instance is skipped as a failover candidate. Fencing the primary stops writes
for the cluster because the rw Service has no endpoint. That is deliberate: use
fencing to freeze an instance for inspection or maintenance, not as a failover
trigger.

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
kubectl cnmysql status cluster-sample
```

Look for entries under `divergedInstances`.

## Restart an instance

Restart all instances in a rolling fashion, or a single instance:

```bash
kubectl cnmysql restart cluster-sample          # rolling restart
kubectl cnmysql restart cluster-sample cluster-sample-2  # single instance
```

The command prompts for confirmation. Skip the prompt with `--yes` or `-y`.

Every instance boots read only. The in-pod role reconciler observes Cluster
status and only clears read-only mode when the instance is the confirmed
primary.

## Destroy an instance

Delete a single instance Pod and its PVC:

```bash
kubectl cnmysql destroy cluster-sample cluster-sample-3
```

This command also prompts for confirmation. Use it to clean up a failed or
diverged instance you have decided to discard. The remaining instances keep
running unaffected.

## Reload MySQL parameters

After you change `spec.mysql.parameters`, apply dynamic parameters without
restarting:

```bash
kubectl cnmysql reload cluster-sample
```

This connects to each instance over mTLS and issues the equivalent of reloading
the running configuration. Parameters that require a restart are noted and need a
follow-up rolling restart.

Update parameters:

```bash
kubectl patch cluster cluster-sample --type merge -p \
  '{"spec":{"mysql":{"parameters":{"require_secure_transport":"ON"}}}}'
```

CNMySQL owns replication, backup, PITR, identity, and lifecycle-critical
settings. User parameters that conflict with managed keys are rejected by the
configuration layer.

## Take an on-demand backup

Instead of crafting a Backup YAML by hand, use the plugin:

```bash
kubectl cnmysql backup cluster-sample
```

This creates a `Backup` object with sensible defaults: `xtrabackup` method,
`prefer-standby` target, online mode. The Backup reconciler then runs the actual
XtraBackup job. Track it:

```bash
kubectl cnmysql status cluster-sample
kubectl get backup -l mysql.cloudnative-mysql.io/cluster=cluster-sample
```

For recurring backups, create a `ScheduledBackup` resource. See the [Scheduled
Backups](./scheduled-backups.md) page for the schedule format and options.

Deleting the `Backup` Kubernetes object does not delete the remote object-store
artifacts today. Remote cleanup is a planned finalizer/retention feature.

## User management

CNMySQL manages MySQL users through the control-tier API, reached over mTLS
port-forwarding inside the cluster:

```bash
kubectl cnmysql user create cluster-sample --name=app --password-stdin < secret.txt
kubectl cnmysql user alter cluster-sample --name=app        # prompt for new password
kubectl cnmysql user list cluster-sample
kubectl cnmysql user drop cluster-sample --name=old-user
```

Passwords are never accepted as flags. Use `--password-stdin` for piping from a
secret, or let the plugin prompt on the terminal with echo disabled.

Users can be created with optional grants (`--superuser`), TLS requirements
(`--require-x509`), and named privileges.

## Database management

Manage MySQL databases the same way:

```bash
kubectl cnmysql database create cluster-sample --name=analytics
kubectl cnmysql database list cluster-sample
kubectl cnmysql database drop cluster-sample --name=analytics
```

You can specify character set and collation on create:

```bash
kubectl cnmysql database create cluster-sample --name=utf8db --charset=utf8mb4 --collation=utf8mb4_unicode_ci
```

## Node maintenance window

Toggle the maintenance window before draining a node or performing Kubernetes
node maintenance:

```bash
kubectl cnmysql maintenance set cluster-sample
kubectl cnmysql maintenance unset cluster-sample
```

Use `--reuse-pvc` to retain the existing PVC across node restarts. This is
useful when the underlying storage is durable and you want to avoid a full clone.

## Scrape Prometheus metrics

```bash
kubectl cnmysql metrics cluster-sample              # primary
kubectl cnmysql metrics cluster-sample cluster-sample-2  # specific instance
kubectl cnmysql metrics -w --filter=mysql_global_status_threads  # watch mode, filtered
```

Add `-w` for continuous refresh. Use `--filter` with a pattern to narrow the
output to matching metric names (grep-style substring match).

## Continuous archiving operations

When continuous archiving is enabled, inspect:

```bash
kubectl cnmysql status cluster-sample
```

Look for `continuousArchiving` in the output. Growing pending files or a
degraded condition usually means an object-store, credential, network, or
throughput issue.

## Safe maintenance habits

- Prefer planned switchover before node or primary maintenance.
- Keep at least three instances for meaningful automatic failover.
- Use semi-sync when acknowledged-write durability matters.
- Keep object-store lifecycle rules aligned with backup and PITR retention.
- Treat retained PVCs and remote backups as recovery assets.
