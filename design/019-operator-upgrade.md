# 019 — Operator Upgrades

**Status:** done

Rolling and in-place instance-manager upgrades decoupled from pod template hash, with PID-based detached supervision and re-exec spike validation.

## Overview

When the operator image is bumped, every instance Pod in every Cluster is deleted and recreated at once. Cause:

- `internal/controller/cluster_pod.go:52-62` — the `bootstrap-controller` init container's `Image` field is the **operator image**. It copies `/manager` into the shared `scratch-data` emptyDir; the `bootstrap` and `mysql` containers then exec that copied binary (`/controller/manager`). The instance manager binary therefore ships *in* the operator image, surfaced to the Pod through the init-container image ref.
- `internal/controller/cluster_resources.go:409-425` — `podAnnotations` folds the full pod spec (including that init container's `Image`) into `podTemplateHashAnnotation`. `restartTriggeringPodSpec` only strips container **Args**, not images.
- `internal/controller/cluster_resources.go:367-369` — `ensurePod` deletes any Pod whose live `podTemplateHashAnnotation` differs from the freshly computed one.

Net: operator upgrade → init image changes → template hash changes for all Pods → simultaneous delete/recreate, no switchover ordering, downtime on every cluster at once.

## Design

### CNPG reference model

Two decoupled steps (see upstream `internal/controller/cluster_upgrade.go`):

1. **Controller upgrade** — apply the new operator manifest. Controller + CRDs + RBAC roll. Instance Pods are *not* touched by this step.
2. **Instance-manager upgrade** — the new controller reconciles each cluster and brings every Pod's instance manager up to its own version, **one instance at a time**, replicas first, primary last via switchover, governed by `primaryUpdateStrategy` (`unsupervised` = auto switchover, `supervised` = wait for user). Optionally **in-place**: stream the new binary to the running Pod and have the instance manager re-exec itself, *no Pod restart, no switchover* (`EnableInstanceManagerInplaceUpdates`, off by default).

Key mechanics CNPG relies on:

- The instance manager reports an **executable hash** in its status (`status.ExecutableHash`). The controller knows its own target hash. Mismatch = that Pod's instance manager is stale → candidate for upgrade.
- The Pod template hash / rollout decision does **not** depend on the operator image, so a controller bump alone does not invalidate every Pod. The bootstrap/operator image change is handled by the dedicated upgrade path (`checkPodBootstrapImage`), which is *skipped entirely* when in-place updates are enabled.
- Rollout is serialized and primary-aware (`rolloutRequiredInstances` → `updatePrimaryPod` → `switchPrimary`).

### What we already have

- `PrimaryUpdateStrategy` (`unsupervised`/`supervised`) and `PrimaryUpdateMethod` (`switchover`/`restart`) already exist in the API (`api/v1alpha1/cluster_types.go:53-76, 187-195`).
- Switchover + failover machinery exists (`internal/controller/cluster_switchover.go`, `internal/controller/cluster_failover.go`), plus `Status.CurrentPrimary` / `TargetPrimary` / `Status.Image`.
- An in-Pod control API on :8080 with a `/status` endpoint and an operator-side `status_client.go` (`pkg/management/mysql/webserver/server.go:91-110`, `pkg/management/mysql/webserver/status.go:34-63`).
- `OperatorImageName` is already plumbed into the reconciler and the plan (`internal/controller/cluster_plan.go:39,221`).

### What was missing

1. The in-Pod `Status` struct does **not** report an executable hash or instance-manager version. Without it the operator can't tell a stale instance manager from a current one — so today it falls back to "image changed → rehash → restart everything."
2. No notion of an instance-manager binary that is separable from the Pod spec's template hash. The init image is part of the hash.
3. No serialized, primary-last rollout *driven by manager staleness* (we serialize config changes, but operator bumps short-circuit straight to mass delete).

## Spike: In-place Re-exec Feasibility

Date: 2026-06-17. Gates Phase 2.

### Question

Can the in-Pod manager replace its own binary and re-exec **without restarting mysqld** (no Pod restart, no switchover)?

### Process model

- `instance run` (`pkg/management/mysql/instance/runner.go`) is PID 1 in the instance container.
- It starts mysqld as a **child** via `exec.Command` + `SysProcAttr{Setpgid:true}` (`pkg/management/mysql/instance/supervisor.go:103-114`), with a goroutine blocked on `cmd.Wait()`.
- mysqld stdout/stderr are wired to pipe-backed `processLogWriter`s (`pkg/management/mysql/instance/process_log.go`); Go holds the pipe **read ends** with `O_CLOEXEC`.

### Result: FEASIBLE ✓

Proved the core OS mechanism in isolation (`sleep` standing in for mysqld; the spike program re-execs itself and re-adopts the child). Output:

```
[start]  manager pid=867579
[start]  started child pid=867585, parent=867579
[start]  re-exec'ing self via syscall.Exec ...
[adopt]  new manager image, pid=867579          <- PID stable across execve
[adopt]  child 867585 still alive after exec ✓  <- child survived
[adopt]  child 867585 ppid=867579 (we are 867579) <- still our child
[adopt]  reaped child 867585 via Wait4 ✓         <- new image supervises it
SUCCESS
```

Why it works:

- `execve()` keeps the **PID** and does **not** kill child processes. mysqld, a direct child of PID 1, survives and stays parented to the (new) PID-1 image.
- Because it stays our child, the new image can reap it with raw `syscall.Wait4(pid, …)` and drive shutdown by `syscall.Kill`. Go's `exec.Cmd.Wait()` cannot wrap a process it did not start, so adoption must use raw syscalls, not `exec.Cmd`.
- `syscall.Exec` does **not** run Go deferreds, so the old manager's `shutdownMysqld` path never fires — mysqld is left untouched.

### Risks surfaced

1. **mysqld stdout/stderr pipe breaks on execve.** Go holds the pipe read ends with CLOEXEC; on execve they close, so mysqld writing to its console fd would get EPIPE/SIGPIPE. **Mitigation:** when in-place upgrades are enabled, wire mysqld's stdout/stderr to **inheritable `*os.File` targets** (e.g. the manager's own fd 1/2, or a real log file) instead of pipe-backed `processLogWriter`s, so the fds survive the swap. Trade-off: lose the structured per-line log wrapper for mysqld output, or re-introduce it differently.
2. **Control/health/metrics listeners (8080/8081/9187) drop during the swap.** Go marks those sockets CLOEXEC, so they close on execve; the new image rebinds. Brief control-API unavailability (not a mysqld outage). Use `SO_REUSEADDR` (Go default for `net.Listen` on Linux) to avoid address-in-use during the rebind. Acceptable.
3. **Child PID handoff.** The new image needs mysqld's PID to adopt it. Pass it via env (e.g. `CNMYSQL_ADOPT_MYSQLD_PID`) set immediately before `syscall.Exec`. Fallback: mysqld pidfile / socket probe.
4. **Replication / role state on re-adopt.** On a normal start, `Run()` runs `EnsureReplicaStarted` / role bootstrap. On *adopt* it must NOT disrupt a running primary or replica — treat the adopt path as "mysqld already configured and serving," skip destructive reconfiguration, just re-open the control connection and resume reconcilers.
5. **In-flight upgrade flag.** Like CNPG's `IsInstanceManagerUpgrading`, set a marker before the swap so any concurrent shutdown path (signal handler, crash watcher) does not kill mysqld mid-upgrade.

### Resulting design for the supervisor

Add an "adopt existing process" mode to `ProcessSupervisor`:

- `AdoptProcess(pid int)` — record the PID, start a `Wait4`-based reaper goroutine that feeds the same `done`/`exitErr` machinery the current `cmd.Wait()` goroutine feeds, so `Wait()`, `Signal()`, `Shutdown*()` all work unchanged for an adopted process.
- `Signal`/`Shutdown*` operate by PID (`syscall.Kill`) rather than via `cmd.Process`, so they work for both started and adopted processes.
- Expose the running child PID (`Pid() int`) so the upgrade endpoint can hand it to the re-exec'd image.

### Minimal vertical slice to prove end-to-end

1. Supervisor: `Pid()` + `AdoptProcess(pid)` (+ unit test reaping an adopted `sleep`, mirroring the spike).
2. `Run()`: if `CNMYSQL_ADOPT_MYSQLD_PID` is set, adopt instead of starting mysqld, and skip destructive replication/role bootstrap.
3. Control endpoint `POST /instance/manager/restart-inplace` (no binary upload yet): sets the upgrade flag, then `syscall.Exec` of `/controller/manager` with the adopt env. Proves mysqld stays up across a manager swap on a real Pod.
4. Only then add binary upload + hash validation (Phase 0/2 plumbing) and operator-side orchestration.

Decision: **proceed to Phase 0 (executable-hash plumbing) and the slice above.** The riskiest unknown is resolved.

## Decoupling mysqld from the manager

Date: 2026-06-17. Prerequisite for the in-place update path (Phase 2). Tracked as its own task: **decouple first, wire the update path next.**

### Why

Today the PID-1 manager supervises mysqld as a **direct child** via `exec.Command` + a `cmd.Wait()` goroutine, with mysqld's stdout/stderr wired to **pipe-backed** `processLogWriter`s (`pkg/management/mysql/instance/supervisor.go:103-129`, `pkg/management/mysql/instance/process_log.go`). That couples mysqld to the manager in two ways that block an in-place manager re-exec:

1. **Pipe fds.** Go holds the stdout/stderr pipe read ends with `O_CLOEXEC`; on `execve` they close and mysqld gets EPIPE/SIGPIPE (spike risk #1).
2. **`exec.Cmd` handle.** Lifecycle, signalling and shutdown all go through `cmd.Process` / `cmd.Wait`, which a re-exec'd image cannot reconstruct for a process it did not start (spike risk #3).

CNPG sidesteps both because postgres is daemonized by `pg_ctl` (not a manager child) and logs to FIFOs. This task brings our long-lived mysqld supervision to an equivalent **out-of-band** model so the later re-exec is trivial.

### Decision

Supervise the long-lived mysqld **by PID**, not by `exec.Cmd`:

- **Inherited output fds.** mysqld stdout/stderr point at the manager's own `os.Stdout`/`os.Stderr` (`*os.File`, no pipe). They survive `execve`, and mysqld keeps its dup'd copies across a manager re-exec, so its log lines keep reaching the container's stdout. Trade-off: we lose the per-line `processLogWriter` structured wrapper for mysqld output (mysqld's error log is already structured). Revisit with a re-openable FIFO later if needed.
- **Pidfile.** On launch the supervisor writes mysqld's PID to `<socketDir>/mysqld.pid` (default `/var/run/mysqld/mysqld.pid`). A re-exec'd image reads it to find mysqld.
- **Signal/shutdown by PID** (`syscall.Kill(pid, …)`), so it works whether the process was started here or adopted after a re-exec.
- **Adopt mode.** `AdoptProcess(pid)` supervises an already-running mysqld via a raw `syscall.Wait4(pid)` reaper feeding the same `done`/`exitErr` machinery as a normal launch. This is what the update path's re-exec'd image calls; it is proven by the spike and lands here so the update task is a thin orchestration layer.

### Scope

- **Only the long-lived runner mysqld** (`pkg/management/mysql/instance/runner.go:252`) moves to the decoupled supervisor. The four short-lived bootstrap servers (`join`, `initializer`, `restore`, `restore_pitr`) keep the existing `ProcessSupervisor` (child + buffered pipe output): they never re-exec and rely on capturing output into a buffer.
- We do **not** use `mysqld --daemonize`. It would force `log-error` to a file plus a tailer to preserve `kubectl logs`, for no extra benefit over inherited fds + PID supervision. mysqld stays a launched-then-PID-supervised process, reparented to PID-1 like any daemon.

### Not in this task

Re-exec orchestration, the in-flight upgrade flag, the control endpoint, binary upload/hash, operator-side rollout. Those are the "update path" task.

### DetachedSupervisor

New `DetachedSupervisor` (own file), same method set the controller/runner use (`Start`, `Wait`, `Signal`, `Shutdown`, `ShutdownWithTimeout`, `ShutdownGraceful`, `Restart`, `Running`) plus `Pid()` and `AdoptProcess(pid)`. `ProcessSupervisor` is left untouched for the bootstrap callers.

- `Start`: `exec.Command(mysqld, args)`, `Stdout/Stderr = os.Stdout/os.Stderr`, `Setpgid`, `cmd.Start()`, write pidfile, goroutine `cmd.Wait()` → `done`.
- `AdoptProcess(pid)`: no cmd; goroutine `syscall.Wait4(pid)` → `done`.
- Shutdown phases reuse the existing SIGTERM→SIGKILL timing, signalling by PID.

## Implementation Phases

Phased. Phase 1 alone removes the "everything restarts" pain; Phase 2 adds the zero-restart in-place path. Primary update defaults to **switchover** (CNPG default): promote a replica first, then upgrade the old primary. Requires >1 instance; single-instance primary falls back to in-place restart.

### Phase 0 — Executable hash plumbing (prerequisite)

- Compute a hash of the running `/manager` binary at startup (read `/proc/self/exe`, sha256).
- Add `ExecutableHash string` to the in-Pod `Status` (`pkg/management/mysql/webserver/status.go:34`) and populate it in the status handler.
- Operator: expose the controller's own target hash (the hash of the `/manager` it would bootstrap into Pods). Simplest: the controller hashes its *own* `/proc/self/exe` — it's the same binary that gets copied via `bootstrap-controller`.
- Operator-side status client: read `ExecutableHash` back per Pod.

### Phase 1 — Decouple controller bump from pod template hash + serialized rollout

Goal: bumping the operator no longer rehashes every Pod; instead the operator recognizes "this Pod's instance manager is stale" and rolls Pods one at a time, primary last.

**1a. Remove the operator/bootstrap image from the restart-triggering hash.**
In `restartTriggeringPodSpec` (`internal/controller/cluster_resources.go:429-443`) normalize the `bootstrap-controller` init container's `Image` (and any field that only carries the operator version) to a constant before hashing, the same way Args are already normalized. Result: an operator bump no longer changes `podTemplateHashAnnotation`, so `ensurePod` stops mass-deleting.

**1b. Add a staleness check + serialized rollout reconcile step.** New file, e.g. `internal/controller/cluster_upgrade.go`, modeled on CNPG's `rolloutRequiredInstances`:
- Iterate instances, replicas first, primary last.
- "Needs rollout" = reported `ExecutableHash != controllerTargetHash` (Phase 0) **or** stale instance image (`pod image != Status.Image`).
- Only roll one instance per reconcile; requeue. Skip fenced instances.
- For the primary: honor `PrimaryUpdateStrategy`. `unsupervised` → trigger switchover via existing `internal/controller/cluster_switchover.go`, then upgrade old primary. `supervised` → set a "waiting for user" phase/condition and stop.
- "Roll" here = delete the Pod (it gets recreated from the new spec with the new operator image in the init container). This is the rolling-update (non-in-place) path.

**1c. Phase/condition reporting** so `kubectl get cluster` shows upgrade progress (mirror CNPG `PhaseUpgrade` / `PhaseWaitingForUser`).

### Phase 2 — In-place instance-manager update (opt-in, zero restart)

Mirror CNPG's `upgradeInstanceManager` + upstream `pkg/management/upgrade/upgrade.go`. Requires the decoupling task (DetachedSupervisor) to be complete.

1. New control-API endpoint on the in-Pod server, e.g. `POST /instance/manager/upgrade` (`pkg/management/mysql/webserver/server.go:91-110`): receives the new `/manager` binary (streamed from the operator), writes it to the shared volume atomically, then re-execs itself so the new manager takes over **without** killing mysqld.
2. Operator streams its own `/manager` to each stale Pod, replicas first, primary last; the primary can be upgraded in place with no switchover.
3. Gate behind a config flag (default off), `EnableInstanceManagerInplaceUpdates`. When off, fall back to Phase 1 rolling update.
4. When on, the rollout checks must *not* treat a bootstrap/operator image change as needing a Pod restart (CNPG's `checkPodBootstrapImage` / `checkPodSpecIsOutdated` early-return when in-place is enabled).

### Suggested PR breakdown

1. Phase 0: executable-hash reporting (in-Pod status + operator client). Small, self-contained, testable.
2. Phase 1a: strip operator image from restart-triggering hash + tests proving an operator bump no longer rehashes Pods.
3. Phase 1b: serialized primary-last rollout reconcile + phase reporting.
4. Phase 2: in-place upgrade endpoint + operator streaming, behind a flag (only after the re-exec spike confirms feasibility).

## Key Source Files

| File | Role |
|------|------|
| `internal/controller/cluster_pod.go:52-62` | bootstrap-controller init container image (operator image) |
| `internal/controller/cluster_resources.go:409-425` | podTemplateHashAnnotation computation |
| `internal/controller/cluster_resources.go:367-369` | ensurePod delete-on-hash-mismatch |
| `internal/controller/cluster_resources.go:429-443` | restartTriggeringPodSpec — Phase 1a normalizes images here |
| `internal/controller/cluster_plan.go:39,221` | OperatorImageName plumbing |
| `internal/controller/cluster_switchover.go` | Switchover machinery used by Phase 1b |
| `internal/controller/cluster_failover.go` | Failover machinery |
| `api/v1alpha1/cluster_types.go:53-76, 187-195` | PrimaryUpdateStrategy, PrimaryUpdateMethod |
| `pkg/management/mysql/webserver/server.go:91-110` | In-Pod control API, Phase 2 endpoint here |
| `pkg/management/mysql/webserver/status.go:34-63` | Status struct — Phase 0 adds ExecutableHash |
| `pkg/management/mysql/instance/runner.go` | instance run entry, PID 1 |
| `pkg/management/mysql/instance/supervisor.go:103-114` | ProcessSupervisor — mysqld child lifecycle |
| `pkg/management/mysql/instance/process_log.go` | processLogWriter pipe wiring |
| `internal/controller/backup_controller.go:286-297` | Backup jobs copy manager from operator image (no change needed) |

## Verification

### Unit
- Phase 0: executable hash computation and status endpoint returns correct hash.
- Phase 1a: pod template hash unchanged when only operator image changes.
- Phase 1b: serialized rollout logic (replicas first, primary last; strategy handling).
- DetachedSupervisor: launch `/bin/sleep`, assert `Pid()`, pidfile written, graceful shutdown; adopt test mirroring the spike (start a child in a helper, `AdoptProcess` its pid, signal + reap via Wait4).

### On-cluster (Kind)
- Operator bump no longer triggers mass Pod delete.
- mysqld comes up under DetachedSupervisor, `kubectl logs` still shows mysqld output.
- Normal shutdown/restart/fence all behave as before.
- In-place manager re-exec: mysqld stays up, Pod `RESTARTS` count stays flat, new manager image re-adopts mysqld and resumes supervision.

### E2E
- E2E tests require an isolated Kind cluster. Validate the full upgrade workflow: controller bump → serialized rollout → cluster health maintained throughout.
