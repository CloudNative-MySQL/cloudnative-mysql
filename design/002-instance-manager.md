# 002 — Instance Manager

**Status:** done
**Milestone:** M2

Build the in-pod instance manager binary that runs as PID1 in every MySQL pod, supervises `mysqld` (Percona Server), bootstraps/joins instances, configures GTID replication, and exposes a control/observability API to the operator over mTLS. No operator-side reconciliation yet.

## Overview

CNPG leans on PostgreSQL's native streaming replication and `pg_basebackup`. MySQL has no equivalent, so the instance manager must actively orchestrate: render `my.cnf`, assign `server-id`, manage `gtid_mode`/`enforce_gtid_consistency`, provision replicas, wire `CHANGE REPLICATION SOURCE`, install semi-sync plugins, and toggle `super_read_only`. This is the MySQL-specific core of the project.

### CNPG Mapping

- `internal/cmd/manager/instance/{run,initdb,join,restore,status,upgrade}` → our subcommands.
- `pkg/management/postgres/{pool,webserver,metrics}` → our `pkg/management/mysql/{pool,webserver,metrics}`.

## Design

### Layout

```
cmd/manager/main.go                         Cobra root for the in-pod binary (separate from operator manager)
internal/cmd/manager/instance/cmd.go        `instance` parent command
internal/cmd/manager/instance/run/          PID1: supervise mysqld + serve control API
internal/cmd/manager/instance/initdb/       Fresh data-dir bootstrap
internal/cmd/manager/instance/join/         Provision a replica from a source
internal/cmd/manager/instance/restore/      Restore from a physical backup (stub wired in M6)
internal/cmd/manager/instance/status/       Print local status as JSON
pkg/management/mysql/
  instance/        Instance abstraction: process supervision, lifecycle (start/stop/restart/shutdown)
  pool/            Connection pool to local mysqld (unix socket) + helpers
  config/          my.cnf rendering from Cluster spec + operator-managed knobs
  replication/     GTID replication setup, role transitions, semi-sync, super_read_only
  webserver/       mTLS HTTP server exposing the control + probe + status API
  metrics/         Prometheus exporter (basic; expanded in M9)
```

### 1. Process Supervision (`instance run`)

- Acts as PID1: starts `mysqld`, forwards signals, reaps children, streams logs to stdout (K8s-friendly).
- Graceful shutdown on SIGTERM: `mysqladmin shutdown` (or `SHUTDOWN` SQL) bounded by `maxStopDelay`; SIGKILL fallback.
- Crash handling: report status; let the pod restart policy + operator decide. Never auto-repromote itself.

### 2. Config Rendering (`pkg/.../config`)

- Render `/etc/mysql/my.cnf` from `Cluster.spec.mysql.parameters` plus **operator-managed, non-overridable** keys:
  `server-id` (unique, derived from instance serial), `gtid_mode=ON`, `enforce_gtid_consistency=ON`,
  `log_bin`, `binlog_format` (from spec), `log_replica_updates=ON`, `relay_log`, `read_only`/`super_read_only`
  for replicas, `report_host`, datadir/socket paths, TLS (`ssl_ca`/`ssl_cert`/`ssl_key`, `require_secure_transport`).
- Validate user parameters against a denylist of keys the operator owns; reject conflicts.
- Version-aware: handle 5.6/5.7 vs 8.0+ keyword differences (e.g. `MASTER`→`SOURCE`, `slave`→`replica`).

### 3. Bootstrap (`instance initdb`)

- Fresh init: `mysqld --initialize-insecure` (or secure + temp password), start, set root password from secret,
  create app database/user (charset/collation), run `postInitSQL`, install required plugins, create the
  replication user (`REQUIRE X509` for mTLS), record initial GTID.

### 4. Replica Provisioning (`instance join`)

- Bring up an empty data dir as a replica of the current primary, then start GTID replication.
- **Provisioning method**: XtraBackup-first (Clone plugin as a later optimization).

### 5. Replication Management (`pkg/.../replication`)

- Configure replica: `CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1`, mTLS source connection, `START REPLICA`.
- Role transitions: promote (stop replica, `super_read_only=OFF`, reset role) / demote (set `super_read_only=ON`).
- Semi-sync: install/enable `rpl_semi_sync_source`/`rpl_semi_sync_replica` plugins driven by `spec.mysql.semiSync`,
  set `rpl_semi_sync_source_wait_for_replica_count` from `minSyncReplicas`.

### 6. Control + Observability API (`pkg/.../webserver`)

- **mTLS** HTTP server (operator authenticates with client cert; instance verifies against cluster client CA).
- Endpoints:
  - `GET /healthz` (liveness: process up) and `GET /readyz` (readiness: accepting conns; replica = replication threads running and lag within bound).
  - `GET /status` → JSON: role (primary/replica), `gtid_executed`, `gtid_purged`, `Seconds_Behind_Source`, `read_only`/`super_read_only`, replication IO/SQL thread state, semi-sync state, mysqld version/uptime.
  - `POST /promote`, `POST /demote`, `POST /restart` — lifecycle commands the operator calls in M3+.
  - `GET /metrics` (or a separate port) for Prometheus.

### Container Image

- `Dockerfile` (or `Dockerfile.instance`) produces an image bundling Percona Server + the `manager` binary as entrypoint.
- Base image: `FROM percona/percona-server` per version.

## Implementation Notes

Suggested implementation order:

1. `cmd/manager` Cobra skeleton + `instance` parent + empty subcommands (build + `--help`).
2. `pkg/management/mysql/pool` (local socket connection helper + interface for tests).
3. `pkg/management/mysql/config` renderer + golden-file unit tests (version-aware, denylist).
4. `instance initdb` + integration test against real Percona.
5. `pkg/management/mysql/replication` (SQL builders + role transitions) with unit tests, then `instance join` (XtraBackup) + integration test (write-propagation).
6. `pkg/management/mysql/webserver` (mTLS, /healthz /readyz /status /promote /demote /restart) + handler unit tests.
7. `instance run` PID1 supervisor + signal/shutdown handling.
8. `Dockerfile.instance` (FROM percona/percona-server) + container smoke check; wire CI.

## Testing

- **Unit (no server):** config rendering (golden files, denylist enforcement, version differences); status JSON marshalling; replication SQL builders (assert generated statements); webserver handlers with a fake `pool`; signal/shutdown logic with a fake process. All mysqld interactions sit behind interfaces.
- **Integration against real Percona (required, Docker-gated):** using **testcontainers-go**, spin up real `percona/percona-server` containers and validate full flows:
  - `initdb` produces a working server with the app db/user and replication user;
  - `join` provisions a replica via XtraBackup and starts GTID replication;
  - a write on the primary appears on the replica (replication actually works);
  - promote/demote toggle `super_read_only` and roles correctly;
  - semi-sync plugins install and acknowledge.
  Guarded by a build tag (e.g. `//go:build integration`) and skipped automatically when Docker is unavailable, but run in CI with Docker. Test matrix covers at least one 8.x and one legacy (5.7) image to exercise version-aware SQL.
- **E2E (in-cluster):** full pod-on-Kind e2e remains M3 (needs the operator to schedule pods). M2's container-level check: image boots, renders config, `mysqld` comes up, `/healthz` & `/readyz` serve.

## Decisions

- Replica provisioning: **XtraBackup-first** (Clone plugin later).
- Transport: **HTTP + mTLS**.
- M2 scope: **real mysqld end-to-end** via testcontainers (Docker-gated integration tests).
- Base image: **FROM `percona/percona-server`** per version.

## Verification

- `cmd/manager` binary builds; `instance` subcommands wired with `--help`.
- Config renderer produces a valid `my.cnf` for primary and replica, version-aware, with denylist enforced — covered by golden-file tests.
- Replication statement builders + status serialization unit-tested.
- mTLS webserver serves `/healthz`, `/readyz`, `/status` against a mocked pool.
- `go build ./...`, `go vet`, `make lint`, `go test ./...` green.
- No changes to operator reconciliation (still none).
