---
title: "Backup Retention and Deletion"
description: "Current Backup deletion behavior, ScheduledBackup owner references, object-store cleanup, and planned retention GC."
sidebar_position: 6
---

# Backup retention and deletion

CNMySQL currently separates Kubernetes object lifecycle from object-store
artifact lifecycle. This is deliberate: a Kubernetes object deletion should not
silently destroy the only copy of a recovery point unless the user explicitly
opts into that behavior.

## What happens today

Deleting a `Backup` object does not delete remote S3-compatible objects.

The remote objects remain under:

```text
<path>/<cluster>/<backup-name>/<backup-id>/backup.xbstream
<path>/<cluster>/<backup-name>/<backup-id>/metadata.json
```

The Kubernetes Job is owned by the Backup, so Kubernetes garbage collection may
remove the Job. Object-store artifacts are not owned by Kubernetes and are not
removed.

## ScheduledBackup owner references

`ScheduledBackup.spec.backupOwnerReference` controls only Kubernetes owner
references on generated Backup objects:

- `self`: generated Backups are owned by the ScheduledBackup.
- `cluster`: generated Backups are owned by the Cluster.
- `none`: generated Backups are standalone.

These modes do not change S3 deletion behavior. If a generated Backup is
garbage-collected, its remote objects still remain.

## Why remote cleanup is not automatic yet

Remote backup cleanup is data-destructive. A Backup object may be deleted
accidentally, by namespace cleanup, by owner-reference cascade, or by a GitOps
prune. Automatically deleting `backup.xbstream` and `metadata.json` in those
cases could destroy the recovery window.

CNMySQL therefore needs an explicit policy before adding remote deletion.

## Planned finalizer behavior

A future Backup finalizer can support remote cleanup. The intended shape is:

1. Add a finalizer to Backups that opt into remote cleanup.
2. On Backup deletion, resolve the recorded bucket/key from Backup status.
3. Delete `backup.xbstream` and `metadata.json`.
4. Record or surface failures so deletion does not silently leave half-cleaned
   state.
5. Remove the finalizer only after cleanup succeeds or the user explicitly
   bypasses it.

This should likely be opt-in at the Backup, ScheduledBackup, or Cluster backup
policy level.

## Planned retention GC

M8.2 is reserved for retention cleanup:

- expire old base-backup archives;
- expire binlog segments that are no longer needed by the retained base backups;
- guard the PITR window so cleanup does not make advertised recovery targets
  impossible;
- surface retention failures in status/events.

Retention must reason about base backups and binlog archives together. Deleting
base backups can make old binlogs unusable, and deleting binlogs can shorten
PITR even when base backups remain.

## What operators should do now

- Use object-store lifecycle rules carefully, and align them with the recovery
  window you need.
- Keep Backup objects for important restore points so their status remains easy
  to inspect.
- Preserve both `backup.xbstream` and `metadata.json`.
- Test recovery before deleting old prefixes manually.
- Document any external cleanup automation outside CNMySQL.

## Manual cleanup checklist

Before deleting remote backup data:

- Confirm no Cluster uses `bootstrap.recovery.backup` for that Backup.
- Confirm no runbook references the backup ID.
- Confirm a newer base backup exists and is restorable.
- Confirm PITR archive coverage still satisfies the required recovery window.
- Delete both the archive and metadata object together.
