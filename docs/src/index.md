---
title: "CNMySQL Documentation"
description: "Architecture and integration notes for operating CloudNative MySQL on Kubernetes."
sidebar_position: 1
---

# CNMySQL Documentation

CNMySQL is a Kubernetes operator for Percona Server for MySQL, designed around
operator-owned lifecycle management, physical backups, failover, and
point-in-time recovery.

## Guides

- [Cluster Lifecycle](./cluster-lifecycle.md): how a `Cluster` becomes Pods,
  PVCs, credentials, TLS material, Services, and status.
- [Replication and Failover](./replication-failover.md): GTID replicas, role
  routing, planned switchover, automatic failover, and former-primary rejoin.
- [Physical Backup and Recovery](./backup-recovery.md): one-shot XtraBackup
  archives, object-store layout, Backup status, and restore flow.
- [Scheduled Backups](./scheduled-backups.md): six-field cron scheduling,
  deterministic Backup creation, owner modes, and retention notes.
- [Point-In-Time Recovery](./pitr.md): architecture, components, recovery flow,
  RPO/RTO model, and operational risks.
