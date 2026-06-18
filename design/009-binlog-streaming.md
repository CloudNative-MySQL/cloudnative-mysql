# 009 — Binlog Streaming

**Status:** done
**Milestone:** M7

Continuous MySQL binary-log archiving to S3 (gapless, GTID-addressable), driven from the instance manager on the primary. Foundation for PITR and ScheduledBackup.

**Goal:** Make continuous MySQL binary-log archiving feel native, the way CNPG
makes WAL archiving native. M6 gave us durable *base* backups; M7 adds the
continuous binlog stream on top of them, so a cluster keeps a gapless,
GTID-addressable record of changes in the same S3-compatible object store. This
is the foundation that point-in-time recovery and `ScheduledBackup` (M8) build
on.

MySQL has no `archive_command` hook like PostgreSQL. The binlog stream has to be
driven from our own instance-manager code in the instance Pods, which is exactly
the kind of "implement it in our manager" work the project plan calls out.

## Scope

### In scope

- **Continuous binlog archiving.** The instance manager on the current primary
  continuously ships completed (rotated) binary-log files to the cluster's
  object store, keeping a gapless GTID-covered archive.
- **Archive object-store layout + per-file manifest.** Deterministic keys under
  a `binlogs/` prefix, each archived file accompanied by metadata (first/last
  GTID, byte size, SHA256, server UUID, timestamps) so recovery can order and
  verify the stream without parsing every file.
- **Purge safety / retention coordination.** Prevent mysqld from purging binlogs
  that have not yet been archived. Surface archive lag so a stalled archiver
  cannot silently let the DB recycle un-shipped logs.
- **Archive status & conditions.** Record on `Cluster.status` the last archived
  file, last archived GTID/time, archiving health, and a `ContinuousArchiving`
  condition. Emit Events on first success and on failure.
- **Failover continuity.** When the primary moves, the new primary's manager
  picks up archiving its own binlog from the correct GTID boundary, so the
  archive stays gapless across a switchover/failover (GTID makes the streams
  composable even though file names/UUIDs differ).
- **Point-in-time recovery (PITR) replay.** Unblock the `recoveryTarget` fields
  that M6 deliberately rejected: bootstrap a base backup, then download and
  replay archived binlogs up to a target time / GTID / "latest", before opening
  the recovered primary for writes.
- **Worker/command plumbing.** Instance-manager commands to run the archiver
  loop in-Pod and to replay a binlog range during recovery. Structured logs,
  never log credentials.
- **Unit, integration, and e2e coverage.** Key construction, rotation
  detection, GTID-range extraction, purge guard, archive status transitions,
  PITR target parsing, and a MinIO-backed Kind e2e: write → archive → PITR to a
  chosen point.

### Out of scope

- `ScheduledBackup` controller and cron semantics (M8).
- Base-backup retention GC tied to binlog retention windows (M8 territory).
- Incremental XtraBackup backups.
- Cross-cluster replica mode / external-cluster live following via the archive.
- CSI `volumeSnapshot` flows.
- Parallel/multi-stream archive sharding; one ordered stream per timeline is
  enough for M7.
- Binlog encryption beyond object-store TLS/SSE (the binlog may already be
  encrypted at rest by mysqld; we treat files opaquely).

## Existing building blocks

- `pkg/management/mysql/objectstore` already builds S3 clients, streams
  upload/download without buffering, and computes SHA256 on the fly
  (`SHA256Reader`/`SHA256Writer`, `ClusterPrefix`, `BuildBackupKeys`).
- `objectstore.BackupMetadata` is an established pattern for a JSON manifest
  written next to an artifact — the per-binlog manifest mirrors it.
- The instance manager already runs as PID1 in each Pod with local filesystem
  access to the datadir (so it can see the binlog directory directly) and an
  mTLS control API + status reader.
- GTID mode is already ON cluster-wide (required since M2/M4 replication), so
  every transaction is GTID-addressable.
- M5/M5.5 give us dynamic roles and a reliable `currentPrimary`, so "archive
  from the primary, follow on failover" maps onto the in-Pod role reconciler.
- M6 recovery already downloads a base backup, extracts/prepares/copy-backs it,
  and reads `xtrabackup_binlog_info` — the natural anchor GTID for replay.

## Decisions

These are the proposals to ratify in review; the genuinely open ones are also
listed under Open questions.

1. **Archive source = local binlog directory on the current primary.** The
   manager watches its own datadir binlog files rather than opening a remote
   `mysqlbinlog --read-from-remote-server` replication connection. It is
   co-located, needs no extra account/connection, and gets bytes verbatim. A
   file is eligible to ship once it is no longer the active log (a newer index
   entry exists), guaranteeing it is complete and immutable.
2. **Archive only from the primary.** Replicas have their own relay/binlogs but
   the authoritative recoverable history is the primary's. The in-Pod reconciler
   gates the archiver loop on `role == primary`. On promotion the new primary
   starts at the GTID frontier the archive already covers; on demotion the old
   primary stops.
3. **One ordered stream, GTID-stitched across timelines.** Keys are partitioned
   by server UUID (which changes on failover) but the per-file manifest carries
   the GTID range, so recovery orders by GTID coverage, not by filename. No
   PostgreSQL-style `.history` file is needed because GTID sets are
   self-describing.
4. **Purge guard.** Set a conservative `binlog_expire_logs_seconds` and refuse
   to let archive lag exceed it: the manager tracks the oldest un-archived file
   and treats falling-behind as a `Degraded` archiving condition rather than
   silently losing data. (Optionally hold purge via not issuing
   `PURGE BINARY LOGS` past the archived frontier where we control purging.)
5. **Compression off by default**, consistent with M6 decision 6, until a
   proven version-compatible toolchain is in the image. Files ship as-is.
6. **Recovery replay uses `mysqlbinlog | mysql`** against the freshly restored,
   not-yet-public primary: download the needed files, compute the replay range
   from the base backup's `xtrabackup_binlog_info` GTID up to the recovery
   target, and stop at the target. `--start-position`/`--stop-datetime`/GTID
   filtering bounds the replay.
7. **Recovery targets:** support `targetTime`, `targetGTID`, and
   `targetImmediate`/"latest". `targetName`/`targetLSN` (PG-isms) stay rejected.
8. **Checksum:** per-file SHA256 in the manifest, same integrity stance as M6
   (ETag is provider metadata, not the source of truth).

## Object-store layout

Continuous archive lives next to base backups under the same cluster prefix:

```text
<path>/<cluster>/binlogs/<server-uuid>/<binlog-file-name>
<path>/<cluster>/binlogs/<server-uuid>/<binlog-file-name>.json   # manifest
```

Per-file manifest (mirrors `objectstore.BackupMetadata` style):

- cluster name, server UUID, source instance name;
- binlog file name and index sequence;
- first/last GTID and the GTID set contributed by this file;
- first/last event timestamp (for `targetTime` recovery);
- byte size and SHA256;
- archived-at timestamp.

A small, periodically-rewritten `binlogs/<server-uuid>/_archive_status.json`
(last archived file + covered GTID set) gives status a cheap per-segment entry
point.

### Cluster-level archive index (multi-failover)

Each `<server-uuid>` prefix is a **segment of one logical timeline**, and a
cluster that has failed over several times has its archive spread across several
UUID prefixes (with overlapping GTID ranges, since `log_replica_updates` makes
each new primary re-emit transactions it received as a replica). Per-UUID status
is too fine-grained for recovery to discover and order that.

A cluster-level index — `<path>/<cluster>/binlogs/_index.json`, updated by the
active archiver on each handoff/rotation — records the timeline as an ordered
list of segments:

- `server-uuid` and its file list / GTID range;
- the **handoff boundary**: the GTID frontier at which authority passed from this
  segment to the next (i.e. the new primary's first authoritative GTID);
- the cumulative covered GTID set across the whole archive.

This is the MySQL-GTID analog of a PostgreSQL timeline-history file. It is needed
not for de-duplication (GTID handles that) but for **discovery and ordering**
across many UUIDs at recovery time. (This refines the earlier "no `.history`
needed" note: GTID is self-describing for dedup, but a cheap index still beats
brute-listing every prefix and inferring order.)

## API and status model

- `Cluster.status.continuousArchiving` (or a `ContinuousArchiving` condition)
  with: enabled, lastArchivedBinlog, lastArchivedGTID, lastArchivedTime,
  lastFailureTime/Reason, and archive lag.
- `Cluster.spec.backup.objectStore` already configures the destination; add an
  opt-in toggle, e.g. `spec.backup.continuousArchiving.enabled` (default off in
  M7 until proven), plus optional `binlogExpireSeconds` / archive-lag threshold.
- Recovery: stop rejecting `spec.bootstrap.recovery.recoveryTarget`; accept
  `targetTime` / `targetGTID` / `targetImmediate` and thread them into the
  restore init path.
- Conditions reuse the `Ready` / `Progressing` / `Degraded` vocabulary already
  used by Backup.

## Reconciliation / runtime design

### In-Pod archiver (instance manager)

1. The in-Pod role reconciler starts the archiver loop only while the instance
   is the current primary and `continuousArchiving.enabled`.
2. The loop watches the binlog index/dir; for each newly-rotated (now inactive)
   file:
   - read its GTID range and timestamps (`mysqlbinlog`/`SHOW BINARY LOGS` +
     event scan);
   - stream-upload the raw file with SHA256;
   - write the per-file manifest and advance `_archive_status.json`;
   - report progress back so the operator can mirror it into `Cluster.status`.
3. On upload failure: retry with backoff, surface lag, never advance the
   frontier past a file that did not land.
4. On demotion/shutdown: flush the active log if safe, ship any remaining
   inactive files, stop.

### Operator side

1. Reconcile archive status/conditions from instance status into
   `Cluster.status` and emit Events (first archive success, archiving degraded).
2. Ensure the primary Pod has what the archiver needs (object-store creds — see
   Open questions — and the `binlog_expire_logs_seconds` setting rendered into
   `my.cnf`).
3. On failover, ensure the newly-selected primary resumes archiving and the gap
   (if any) is visible in status.

### Recovery (PITR) bootstrap

1. Extend M6's `bootstrap.recovery` path: after base-backup
   extract/prepare/copy-back, if a `recoveryTarget` is set:
   - read anchor GTID from `xtrabackup_binlog_info`;
   - load the cluster-level `_index.json` and compute the **timeline path** from
     the anchor GTID to the target across however many UUID segments it spans;
   - **replay timeline-by-timeline in handoff order** (segment DB1, then DB2,
     …), downloading each segment's files and feeding `mysqlbinlog | mysql`;
     MySQL's GTID engine **auto-skips** the overlapping transactions each
     successor re-emits, so replaying the union in order is self-deduplicating;
   - stop at the target time/GTID on the final segment;
   - refuse to straddle a **forked** timeline (segments that diverge rather than
     nest — which can only happen if seam-contiguity was already flagged
     `Degraded` at archive time); pick a single coherent timeline or fail loudly;
   - then open the instance as the first primary via the M5.5 role flow.
2. Replicas clone from the recovered primary using the existing M4 path.
3. Reject targets the archive cannot satisfy (target older than the base backup,
   or beyond archive coverage) with a clear condition.

## Command/package design

Extend `pkg/management/mysql/objectstore` with binlog key construction
(`BuildBinlogKey`, `BinlogPrefix`) and a `BinlogMetadata` manifest type, mirror
of `BackupMetadata`.

Add a binlog package, e.g. `pkg/management/mysql/binlog`:

- watch/enumerate the binlog index and detect rotation;
- extract GTID range and timestamps for a file;
- compute the replay range between two GTID points and drive
  `mysqlbinlog | mysql`.

Instance-manager commands under `internal/cmd/manager/instance`:

- `binlog archive` (or fold into the run loop): the continuous archiver;
- `binlog restore` / `pitr`: download + replay a bounded binlog range during
  recovery.

Structured logs throughout; binlog payloads are a data path (per
the project logging decision (only stderr becomes structured logs),
credentials and signed URLs always redacted.

## Security

- Continuous archiving forces a change from M6 decision 2 (no S3 creds in DB
  Pods): something co-located with the active binlog must push to S3 on an
  ongoing basis. See Open questions for sidecar-vs-instance-manager. Whichever
  wins: scope the Secret tightly, owner-reference nothing unexpected, redact in
  logs/status, and keep the blast radius to object-store write on this cluster's
  prefix.
- mTLS unchanged for any control-plane calls; the archiver itself works on local
  files, not the mTLS stream.

## Implementation order

1. Object-store binlog keys + `BinlogMetadata` manifest + `_archive_status.json`
   helpers, with unit tests.
2. `pkg/management/mysql/binlog`: rotation detection + GTID/timestamp extraction
   over a real-Percona integration fixture.
3. In-Pod archiver loop gated on primary role; `binlog archive` command.
4. Purge guard / `binlog_expire_logs_seconds` rendering + archive-lag threshold.
5. Operator: mirror archive status/conditions/Events into `Cluster.status`;
   `continuousArchiving.enabled` toggle.
6. Failover continuity wiring + tests.
7. PITR: replay-range computation, `binlog restore`/`pitr` command, unblock
   `recoveryTarget` in the recovery bootstrap.
8. Unit tests across objectstore/binlog/controller/commands.
9. Integration (MinIO/testcontainers): archive a rotated file round-trip;
   replay a fixture range.
10. Kind e2e: cluster with archiving on → write, force rotations → assert files
    + manifests land → bootstrap a fresh cluster with a `targetTime`/`targetGTID`
    between two writes → verify it recovered to exactly that point and replicas
    scale up.

## Testing

- Unit:
  - binlog key/prefix construction and manifest formatting;
  - rotation/eligibility detection (active vs inactive file);
  - GTID-range and timestamp extraction parsing;
  - replay-range computation between anchor GTID and target (time/GTID/latest);
  - archive-lag / purge-guard threshold logic;
  - archive status/condition transitions;
  - recoveryTarget parsing + rejection of unsatisfiable/PG-only targets.
- Integration:
  - MinIO round-trip of a rotated binlog file + manifest;
  - `mysqlbinlog | mysql` replay against a real Percona fixture to a target.
- E2E:
  - archiving cluster ships files across at least one forced rotation and one
    failover, archive stays gapless by GTID;
  - **crash primary mid-segment:** primary is part-way through an un-rotated
    binlog (e.g. ~8 MB in `binlog.000004`), kill it, let a replica promote;
    assert (a) every acknowledged transaction is still in the archive — re-keyed
    under the new primary's UUID — (b) the archive replays to "latest" with no
    GTID gap, and (c) no `Degraded` gap condition is raised. Relies on
    `log_replica_updates = ON` and durable semi-sync;
  - **collision guard:** force a duplicate `server_uuid` (or a `RESET MASTER`)
    and assert the archiver refuses to clobber an existing key and raises a loud
    condition instead of silently overwriting;
  - PITR bootstrap recovers to a chosen point between two known writes (sees the
    first write, not the second) and serves data;
  - **multi-failover PITR:** drive the cluster through ≥2 failovers (archive
    spans ≥3 UUID segments with overlapping GTID ranges), then PITR to a target
    that lands on the *last* segment; assert the timeline-path replay applies
    every segment in order, GTID auto-skip handles the overlaps, and the result
    matches the expected data exactly;
  - restored cluster scales replicas via the existing clone path.

## Acceptance criteria

- With `continuousArchiving` enabled, a cluster continuously ships rotated
  binlog files + manifests to the object store, gapless by GTID, including
  across a failover.
- `Cluster.status` exposes last archived file/GTID/time and a meaningful
  archiving condition; archive lag is visible and a stalled archiver is loud.
- mysqld cannot purge a binlog the archive has not captured without it showing
  as degraded.
- A new cluster can bootstrap from a base backup + archived binlogs and recover
  to a chosen `targetTime` / `targetGTID` / latest, serving exactly the expected
  data.
- Unsatisfiable or PG-only recovery targets fail loudly with useful conditions.
- `make generate manifests`, `make lint-fix`, `make test`, integration tests,
  and the M7 MinIO-backed PITR e2e pass.

## Resolved decisions (review round 1)

1. **Credentials live with the archiving process.** The archiver runs co-located
   with each instance (see "Streaming process" below) and the object-store
   Secret is mounted into that process only. There is no separate remote
   archiver pulling over the network, so there is no second cred location.
2. **The manager owns binlog purge.** We actively gate `PURGE BINARY LOGS` so
   mysqld never recycles a binlog past the archived frontier. A cluster/instance
   **annotation escape hatch** lets an operator force purge (and accept the data
   loss) when they explicitly want it.
3. **M7 splits into M7.1 and M7.2.** M7.1 = continuous binlog archiving (the
   streaming foundation, status, purge guard, failover continuity). M7.2 = PITR
   replay / `recoveryTarget`. M7.1 is independently shippable and useful (durable
   change history); M7.2 builds on it.
4. **RPO-bound by forced rotation, configurable.** A periodic `FLUSH BINARY LOGS`
   plus a size trigger bound RPO so a low-write cluster still archives promptly.
   Defaults follow the PostgreSQL spirit: **16 MiB or 5 minutes**, whichever
   comes first, both tunable.

## Streaming process

The mechanism has to be **safe** (never declare data archived that is not
durably in S3, never let mysqld purge un-archived logs), **failover-tolerant**
(if the archiver or the whole server dies, archiving continues from the new
primary and the overall archive stays usable), and **relatively fast** (low,
predictable RPO).

### Shape: co-located, primary-gated, local-file archiver

- The archiver runs **in every instance Pod** as part of the instance manager
  (its credentials and binlog-dir access live right there — resolution #1), but
  it **actively archives only while that instance is the current primary**,
  gated by the M5.5 in-Pod role reconciler. On failover the new primary's
  archiver activates; a fenced/demoted old primary's archiver stops.
- It archives by **reading completed (rotated, now-inactive) local binlog
  files** from the datadir, not by opening a remote replication stream. Since it
  is co-located it already has the bytes mysqld wrote; the replication protocol
  would only add a connection, a fake `server_id`, and a second failure mode for
  no gain. (A remote streamer only makes sense for an out-of-pod archiver, which
  we rejected with resolution #1.)
- RPO is bounded by **forced rotation** (resolution #4): a size/time trigger
  issues `FLUSH BINARY LOGS`, turning the active segment into an immutable
  archivable file on a predictable cadence.

### Why this is safe

- **Only immutable segments are uploaded.** A file is eligible only once it is no
  longer the active log (a newer index entry exists). The active tail is never
  shipped mid-write.
- **Commit order: bytes → manifest → frontier.** Upload the raw file (streaming,
  SHA256 on the fly), then write its `.json` manifest, then advance
  `_archive_status.json`. A crash between steps leaves a file with no manifest,
  which the next pass treats as not-archived and retries. Keys are
  `server-uuid/binlog-name`, naturally unique and stable, so retried uploads
  overwrite identical bytes — **idempotent, effectively exactly-once**.
- **Purge gate (resolution #2)** means mysqld physically cannot recycle a segment
  the archive has not captured, except via the explicit annotation override.

### Why failover stays correct — GTID is the unit, not the file

The archive's completeness invariant is defined over the **GTID set**, not over
any single server's file sequence: *the union of archived per-file GTID ranges
must cover, with no gaps, the GTID set we want to recover to.*

- Each instance has a distinct server UUID, so its binlogs key under
  `binlogs/<server-uuid>/...` and two archivers can never clobber each other.
- **`log_replica_updates = ON` is mandatory cluster-wide.** It makes a replica
  write every *replicated* transaction into its own binlog, so when that replica
  is promoted its binlog already covers the GTID history it received as a
  replica. The new primary's archiver therefore continues the GTID sequence with
  no gap, under a new UUID.
- **Semi-sync (M5) defines the contract.** A transaction acked to the client was
  durable on at least one replica, so the post-failover primary has it and will
  archive it. The crashed primary's un-archived active tail only ever held
  *un-acked* transactions, which were never durable — so no acknowledged data is
  lost. The archive composes across the failover.
- **Overlap is fine, gaps are loud.** After failover the new primary's archive
  may re-cover GTIDs an old primary already archived (it replicated them);
  recovery de-duplicates by GTID (`--exclude-gtids` of the already-applied set),
  so overlap is harmless. A genuine *gap* (new frontier not contiguous with the
  prior cumulative set) is surfaced as a `Degraded` archiving condition rather
  than silently accepted.
- **Single writer per timeline.** Strict gating on `currentPrimary` plus existing
  M5 fencing keeps a demoted/fenced node from continuing to push. Even a brief
  overlap is safe because of UUID-scoped keys + GTID de-dup.
- **Clean switchover flushes the tail first.** On a planned switchover, force a
  final `FLUSH BINARY LOGS` + archive of the old primary's tail before completing
  promotion, so even the last segment lands rather than waiting on the cadence
  timer.

### Filename-collision safety (server_uuid uniqueness)

Binlog basenames are sequential per server (`binlog.000004`) and not globally
unique, so two primaries will both produce a `binlog.000004`. Keys are
partitioned by `server-uuid`, which keeps those apart — **provided `server_uuid`
is genuinely unique**. Two ways that can break, both guarded:

- **Cloned datadir reusing a UUID.** XtraBackup copies `auto.cnf`, so any
  clone-provisioned instance (replica join, recovery bootstrap) inherits the
  source's `server_uuid` unless `auto.cnf` is deleted to force regeneration. A
  shared UUID would let two servers write differing `binlog.000004`s to the same
  key. This is already a **replication-correctness requirement** (duplicate
  `server_uuid` breaks GTID replication), so the M4 join / M6 restore paths must
  regenerate it; M7 hard-depends on that and verifies it.
- **`RESET MASTER` resets the index to `000001`**, so the same UUID could emit a
  second, different `binlog.000001`. Healthy archiving primaries are never reset,
  but our own bootstrap can issue it, so `RESET MASTER` is **guarded** on an
  archiving cluster (refuse / repartition), same posture as the purge gate.
- **Defense in depth: never blind-overwrite.** Before upload, if the target key
  already holds an object with a *different* SHA256 / GTID range, fail loudly
  (`Degraded`) rather than clobber. A correct retry is byte-identical
  (idempotent); a differing object at the same key means a uniqueness invariant
  broke and must surface, not be silently lost.

### Why it is fast enough

- Uploads stream without buffering the whole file and checksum on the fly (reuse
  M6's `SHA256Reader`); multiple pending rotated files can upload concurrently
  while the cumulative frontier advances in GTID order.
- RPO is the forced-rotation cadence (default 16 MiB / 5 min), tunable down for
  tighter RPO at the cost of more, smaller objects.

### Required server settings (rendered into `my.cnf`)

`log_bin` on, `gtid_mode = ON` (already), `enforce_gtid_consistency = ON`,
`log_replica_updates = ON` (the failover-continuity linchpin),
`binlog_format = ROW`, `sync_binlog = 1` for durability, a unique `server_id`
(already), and a conservative `binlog_expire_logs_seconds` as a backstop under
the active purge gate.
