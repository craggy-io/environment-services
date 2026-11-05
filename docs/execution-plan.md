# Execution Plan — environment-services full cutover

Module: `github.com/craggy-io/environment-services`
Source: `cclab-environments` at `249c34d` (149 Go files, ~33k LOC, 14 packages under `src/`).
Consumers cut over in the same programme: `cclab-gates`, then `cclab-platform`.
Design authority: [plan.md](plan.md), as amended by the decisions below.

Scope is all three stages in one programme:

1. Relocate `cclab-environments` into this repo with the standard Craggy layout.
2. Replace direct Starlark with `github.com/craggy-io/script-services`.
3. Implement the durable lifecycle redesign from `plan.md`.

Sub-goal throughout: **reduce complexity and public surface**. Delete, never deprecate.
One repo only — no extractions (`plan.md` §9 is deferred entirely).

## 0. Governing decisions — ratify before code

| # | Decision |
|---|---|
| D1 | Clean drain. Fresh state DB, new `craggy.*` labels/names. No adoption, migration, or compat `state` projection. Amend `plan.md` §7 (compat clause), §8 (preserve identity), §10 (incremental sequence) — all three contradict this programme. |
| D2 | Durable operations only for `provision \| release \| reset \| attach`. Exec/Push/Pull/device actions stay ephemeral, fenced by a handle epoch check. No second job system beside capability-services jobs. |
| D3 | Shutdown ships `Drain \| Disconnect` only. `Retain` deferred until an adapter can verifiably preserve. Crash recovery of pending operations is separate and mandatory. |
| D4 | Physical Android is new scope. `Attach` is in the adapter contract from Stage 3; the physical adapter is a late, droppable stage (5b). |
| D5 | Stream JSON frozen at current versions (`android.crash@2` etc.). No field renames. Golden decode of pre-cutover captures is a release gate. Gates domains may alias new record types or keep mirrors; aliasing is not mandated. |
| D6 | No `Overview` god-struct on the facade. Platform renders from `Profiles/List/EventsSince/HostSnapshot` plus envcap JSON capabilities it already shapes. |
| D7 | Generic `collect` package deleted. Each adapter owns its collectors. `internal/signalcore` is hub/recorder/range only. |
| D8 | Artifact port changes to `Stage/Keep` in v0.1.0, no `Put` shim. Cross-repo atomicity is handled by the pre-tag pseudo-version freeze (see §6). |
| D9 | **Deviation needing ratification:** public surface is 3 production packages (`envspec`, `environments`, `envcap`) **plus** `envtest`, a chartered test-only package. Consumers' tests cannot import `internal/`, and platform/gates tests currently publish through a real `signal.Hub` and hand-assemble collectors. `envtest` ships `FakeObservation` instead of a collector-authoring API. |

## 1. Why the facade can shrink this far (evidence)

Measured on the old repo (`go list` + `go doc -all` + type-resolved consumer scan):

- Exported declarations today: `collect` 160, `env` 128, `signal` 100, `android` 97,
  `envtest` 50, `vault` 49, `envspec` 40, `rules` 40, `docker` 32, `xfer` 29,
  `hostenv` 27, `host` 14, `envcap` 9.
- External packages actually named by gates+platform: `docker` 1 (`New`), `hostenv` 1
  (`Default`), `vault` 3, `xfer` 4. `android/adb`, `envtest`, `xfer/agent`: zero external importers.
- Methods externally unused: `Manager` 17/35, `Env` 16/26, `Hub` 13/22, `Device` 5/21 used
  in production scan subset (14 production methods verified for gates), `vault.Vault` external
  surface is only `Close/Leases/Redactor`.
- Dead everywhere, delete outright: `env.TargetFor`, `signal.WritePCAP`,
  `signal.WithBackpressure`, `vault.WithEvents`, `android.Device.InvokePackageAction`.
- Zero external use, delete from new `envspec`: `Provisioner`, `Runner`, `DigestSink`,
  `ConcurrentSink` (move to `internal/xfer`), `Entry.IsSymlink`, `ServiceLease.ID`.
- Everything else externally unused is reachable internally (via `envcap`/`env`/`collect`):
  **internalize, do not delete**. A `deadcode -test ./...` pass re-verifies before any deletion.

Dependency findings that shape the layout:

- No Go import cycles, but three bridge tricks hide them: `signal.Target.Handle(key) any`,
  `env.Extra() any`, and duplicated `env.Cmd` vs `signal.Cmd/Proc`. Collectors type-assert
  concrete `*moby`/`*hostenv.Instance`/`*adb.Client` through those holes.
- `env` is a god package (20 files, ~5.2k LOC: manager + sqlite store + provision + leases +
  admission + services + transfer). Do not port it file-by-file.
- `examples/demo` (fan-out 11) is the only in-repo assembly root; the real one is platform's
  `openEnvironments`. The new `environments.Open` replaces both.

## 2. Target layout (mirrors script-services: flat root packages, docs per area)

```text
environment-services/
  go.mod            module github.com/craggy-io/environment-services
                    requires capability-services v0.1.0, script-services v0.1.0 exactly
                    go.starlark.net direct only for internal/rules
                    NO artifact-services dependency
  README.md / AGENTS.md / LICENSE   (Craggy branded README; import rules in AGENTS)
  arch_test.go      layering enforcement (see §4)
  docs/             plan.md (amended), execution-plan.md (this file), cutover.md (Stage 10)
                    + package guides as they land

  envspec/          PUBLIC. Stdlib-only records + service interfaces.
  environments/     PUBLIC. The one assembly/manager package.
  envcap/           PUBLIC. Capability surface over envcap-owned interfaces.
  envtest/          PUBLIC test-only. FakeObservation; doc.go charter: no prod imports.

  internal/store/        SQLite schema + typed transitions (was env/state.go)
  internal/core/         durable lifecycle, leases, admission, ops, conditions, activities
  internal/adapter/      adapter contract only (was env/driver.go)
  internal/adapters/docker/      was docker/ + its collectors (logs, pcap/http)
  internal/adapters/hostenv/     was hostenv/ + fs/resources collectors
  internal/adapters/androidemu/  emulator lifecycle (boot/prepare/proc/magisk/ramdisk)
  internal/adapters/androidphys/ NEW physical attach (Stage 5b, droppable)
  internal/androiddev/   device-over-serial (device/frida/supervisor); behind AndroidDevice
  internal/adb/          was android/adb; no internal deps
  internal/signalcore/   hub/recorder/capture/range/filter (signal minus Target/Cmd/Proc)
  internal/vault/        own sqlite file; Secret.Reveal never leaves internal/
  internal/hostmon/      was host/; pure sampler
  internal/xfer/         was xfer/ minus Mem* (moved to envspec)
  internal/rules/        rewritten on script-services (see §5)
  internal/envtestfake/  fake adapters for in-repo core tests
  cmd/xfer-agent/        was xfer/agent
  examples/demo/         imports environments+envcap+envspec only
```

## 3. Minimal public API

### 3.1 `envspec` (stdlib-only — load-bearing for gates domains)

Keep verbatim: `Spec, Profile, Build, Resources, Kind (+consts), Network (+consts),
Owner, Entry (+IsDir), Source, Sink, Progress, NewID, AndroidOptions, ServiceSpec,
ServiceRequest, ServiceInfo, ServiceProvider`.

Breaking change, accepted: `Handle` becomes
`{ID, Info, Exec(ctx, Cmd) (stdout, stderr, exit, err), Push, Pull, Release}`;
`ServiceLease` embeds `Handle + {ServiceInfo, Lock}`. Old names
`ExecHandle/PushFiles/PullFiles` deleted. Platform edits: `core/ghidra`, `core/envjob`,
`application/ghidra.go`, `service_test` fakeLease.

Moved in as records (verbatim fields/JSON, replacing `collect/signal/env/host/xfer` leakage):

- `Cmd{Argv, Env, Timeout}` — the one Cmd (deletes `env.Cmd`, `signal.Cmd/Proc`).
- `TransferStats`, `MemSource`, `NewMemSink/MemSink`.
- `Info` (was `env.Info`) + `Conditions []Condition`; `Condition{Name, Status, Reason, ObservedAt, Detail}`.
- `LifecycleEvent` (was `env.Event`); seq cursor and owner/session/job attribution preserved.
- `HostSnapshot`, `HostCapacity`, `PoolCapacity`.
- `ProfileStatus{Profile, Ready, NotReady, State}` — deletes ~120 lines of platform readiness wrappers.
- Signal records: `SignalEvent, Selector, Quality, Coverage, RangeMeta`.
  `Coverage/Quality` are persisted inside gates JSON, so they are records, not live API.
- Payloads: `StateEvent/MemoryEvent/CrashEvent/IPCEvent/FridaData` + param types
  (`StateParams, MemoryParams, FridaParams`) + `FridaCheckpoint/FridaReady` + stream-name consts.
  Schemas are generated by reflection inside internal collectors; golden tests pin the JSON.
- Android receipts: `PackageMutation/Action(+Receipts), BackupReceipt`, `PendingIntentProtocol`.
  TLS/Cleartext challenge code and the PendingIntent observer script move **to gates**.
- Sentinels: `ErrNotFound, ErrInvalid, ErrNotReady, ErrConflict (new), ErrBackupRejected, ErrBackupRecipientEmpty`.
- `MutationPolicy` interface (new): `AllowMutation(envID, kind) (bool, string)` — the experiment-hold port.
- `SignalRequest.Required` (new field; default false = today's best-effort).

Verify-then-delete in Stage 1: `IOS` kind (no driver exists).

### 3.2 `environments` (assembly)

`Open(ctx, Config) (*Manager, error)` builds hostmon + vault + signalcore + rules + adapters
(host always; docker if binary present and backend check passes; android iff SDK configured).
Replaces platform `openEnvironments` (~150 lines → ~20) and gates `testenv` hand-assembly.

`Config{Dir, ManagerID, AndroidSDK, FridaServerDir, HostSampler, Profiles, HostPaths,
MutationPolicy, Logger}`.

Manager methods (each justified by a gates/platform call site):
`Close(ctx, ShutdownDrain|ShutdownDisconnect)`, `Provision` (+`OnProgress`,
`WithRequestKey`), `Get`, `Release`, `List`, `Profiles`, `Profile`,
`EnsureProfileBackend`, `Events/EventsSince/RecentEvents`, `AcquireService/StopService`,
`RenewLease` (new — the sole holder-renewal path), `Android(id)`, `Observation()`,
`HostSnapshot`, `RedactString`, `Drivers`, `StockProfiles`, `BuildDigest`, `ManagerID`,
`Warnings` (new — reports foreign `cclab.*` orphans at startup; never adopts them).

Deleted from the facade (capability-internal now, reached via envcap's own interface):
`Borrow/RenewBorrow, Cursor/RegisterCursor, Notices, Keepalive, RunOnce, Transfer,
ReleaseJob/ReleaseSession, PrepareProfile/RegisterProfiles, ReleaseAll, Emit/Notice`,
and `Env.{ExecProc, Start, Snapshot/Restore, Manifest, List/Stat, Workspace, Instance, Extra}`.

`AndroidDevice` interface (replaces `Extra().(*android.Device)`): `Serial, Shell, RootShell,
Install, Uninstall, StartApp, ForceStop, Tap, Swipe, KeyEvent, Text, PackageSnapshot,
ApplyPackageSequenceAndAction, BackupToLocalTransport, Logcat, Screenshot`.
Excluded: `TypeSecret` (envcap-internal; takes a vault lease).

`Observation` interface: `Pin/PinFresh` (implemented as activity restart-with-generation),
`ConfiguredState, Query, ConfiguredCoverage, Running, Capture`.
`Capture{ID, Source, Close}`.

### 3.3 `envcap`

Keep today's 9-declaration shape; fix `Deps`. `envcap` does **not** import `environments`:
`Deps.Manager` is an envcap-owned interface listing exactly what handlers call (including
capability-internal ops absent from the consumer facade), satisfied structurally via
`environments.NewEnvCapDeps(...)`, with the compile-time assertion in `environments`.
Envcap is inside the trust boundary — state that in its `doc.go`.
Deleted `Deps` fields: `Vault, Hub, Rec, Rules, HostSnapshot, BorrowAuthorised`.

Records: `Operation{ID (durable, required — today never set), Capability, JobID (registry
job via JobIDFrom — today wrongly owner.Job), Parent (parent op id via ctx), Env,
Generation, LeaseEpoch, Inputs}`, `Evidence` (verbatim), `Ref`,
`ArtifactRun{Source, Stage, Keep, Record}`, `Artifacts.Run(...) (map[string]Ref, error)`
(error may wrap `PartialOutputs`). `Put` and `ProgressReport/ReportProgress` deleted.

Registry jobs remain a projection: on restart the registry job is `lost`, the env
operation resumes; a retry `Invoke` mints a new job bound to the same op.

### 3.4 `envtest` (test-only)

`FakeObservation` (implements `Observation` + `Publish(SignalEvent)`), optional
`FakeServiceProvider`. No collector-authoring API. Gates fake-collector tests convert
to `FakeObservation` + JSON goldens.

## 4. Internal layering + enforcement

Dependencies point one way (importer → imported):

```text
envspec (stdlib) <- {adb, hostmon, xfer, signalcore, adapter} <- vault <- core
  <- {adapters/*, androiddev, rules} <- environments
envcap -> {envspec, capability-services/registry} only
```

Rules: adapters import `adapter` + `envspec` + their own tech (moby/gopacket/adb) and
register collectors against a collector contract in `signalcore` (imports only `envspec`).
Collectors receive typed ports from **their own adapter** — never cross-adapter concrete
types, never a stringly `Handle(key) any`. `rules` → script-services + signalcore.
Envcap must not import script-services/starlarkcap; consumers assemble the Starlark surface.

Enforcement: `arch_test.go` at repo root using `go list -json`:
`TestEnvSpecStdlibOnly` (ported) and `TestLayering` (forbidden edges: adapters→core/
environments/envcap/each-other; signalcore→adapters/core; core→adapters/moby/gopacket;
envcap→environments/internal/; any→artifact-services; go.starlark.net and script-services
only in `internal/rules`; capability-services only in envcap+demo).

## 5. Durable core (smallest design that satisfies plan.md §1–§7)

One SQLite file (env DB) + vault's own file. `MaxOpenConns(1)`, WAL, single-manager
file lock kept. Five tables + events:

- `envs(id, kind, profile, spec, owner, generation, revision, desired[present|released],
  phase[acquiring|present|releasing|released|failed], presence[present|absent|unknown],
  instance, image, warm, created)`
- `operations(id op_, env, generation, kind, request_key, request_digest, phase, attempt,
  next_attempt, deadline, outcome[pending|ok|failed|unknown], external_ids, error)` +
  UNIQUE partial index `(owner_scope, request_key)`. No finalizer table:
  `desired=released` + open release op **is** the cleanup obligation.
- `leases(env PK, lease_id, epoch, holder, expires, state)` + `borrows` kept.
- `conditions(env, generation, name PK, status[T|F|Unknown], reason, observed_at, detail)` —
  freshness computed at read against a per-name TTL constant. No freshness framework.
- `activities(env, generation, name PK, kind[probe|collector|component|service], desired,
  required, state, attempts, next_attempt, last_ok, last_error, params)`
- `events` kept exactly (state+event in one tx — the best existing pattern).

Mechanisms:

- All updates are typed transitions `... WHERE id=? AND revision=?`, `RowsAffected==1`
  else `ErrConflict`. A failed durable write aborts the operation and closes admission
  (`storeHealthy` gate). Replaces map-based `updateEnv` and every `_=` error discard.
- Provision: tx1 inserts env(acquiring) + operation + lease(epoch 1) **before** dispatch;
  deterministic external names derived from env id (`craggy-` prefix) are the rediscovery
  key; tx2 records the external id immediately on Create/Attach return. Recovery
  re-observes pending ops. Handle issued **last**, after readiness barriers:
  admission → create/attach → identity verify → creds (`creds_ready`) → required
  activities acked → `present`. Fixes ready-before-creds/pins.
- Idempotency: insert op first; digest = sha256(canonical merged spec+owner). Same digest
  → same op (survives restart); different → `ErrConflict`. Deletes `findByKey`.
- Handle = value `{EnvID, Generation, LeaseID, Epoch}`; one `checkHandle()` helper at
  Exec/Push/Pull/Release/RenewLease/Android. The runtime env object never escapes and is
  never reused across holders (warm claim = conditional UPDATE + generation/epoch bump +
  fresh runtime object after Reset).
- Release: stop activities (ack/timeout) → vault `ReleaseHolder` (error recorded) →
  Destroy|Disconnect → verify `Absent` → only then `released` + reservation returned.
  Destroy failure or non-Absent → op pending, backoff, retried across restarts.
  Release during provisioning: `desired` wins; late Create result feeds the release op.
- Admission: reserved = SUM(cost where phase in acquiring|present), recomputed at Open.
  Deletes `releaseRes` closures and the reconcile capacity leak by construction.
- One activity reconciler (backoff 1s→30s) drives collectors/components/services/probes.
  Collectors run once and return error (per-collector retry loops deleted). Hub
  refcount/linger deleted as lifecycle owner; pins are activity rows. Android Supervisor
  probe/repair/suspect/capture-before-repair kept as component-activity implementation,
  gated by `MutationPolicy` (denied → `held`, reason `experiment_hold`, event emitted).
- Services are `kind=service` activities; Ghidra's three identities (service generation,
  borrower lease, transaction `Lock`) stay distinct. `StopService` is generation-fenced.
- Heartbeats separated: holder renewal only via `RenewLease` (renew-on-use and RunOnce
  self-renew deleted; platform envjob owns job liveness and calls it). Probes write
  conditions. Job progress stays platform-side.

Adapter contract (`internal/adapter`, every call takes `OpContext{OpID, EnvID, Generation}`):
mandatory `Describe/Validate/Observe/Discover`; optional `Creator, Attacher, Executor,
Transferrer, Snapshotter, Preparer, ImageBuilder, CredInjector, FSLister` (used subset of
today's 12; docker's stub `Snapshotter.Load` deleted). Presence mapping is explicit:
backend-down → `Unknown` (reconcile no-ops, never invents absence); definitive not-found
→ `Absent`. Dispositions: `Destroy | ResetToPool | Disconnect | RetainWorkspace`.
Physical (5b): Discover = non-emulator serials; Attach verifies serial + `state==device`;
Detach removes forwards/Frida only; no Create/Destroy/Reset.

Deleted for complexity: `StateQueued`, `grace_until`/recovery grace, notices table,
cursors table + prune coupling, named-daemon registry, three teardown copies → one
release reconciler, `provisionWarm` duplicate body, `targetAdapter/Target.Handle/Extra()`,
`env.process@1` stub, duplicate host-metrics path, `android/instances/*.json` dual store
(DB `external_ids` is authority), hostenv recreate-empty-job adoption.
Cut from `plan.md` as over-engineered for single-host: persisted substates, resource
graph (`owns/borrows/depends_on`), controller heartbeat, freshness-as-data,
remote-executor epochs.

## 6. Rules on script-services; artifacts

Rules (`internal/rules`): keep Engine/instance mailbox (4096), depth-4 loop guard,
derived-stream anti-forgery, fault-disable, hot reload, virtual-clock `Test`, helpers.
Replace only the execution mechanics:

- Load: `check.Analyze` advisory (reject any `load()`; `check` always predeclares `input`,
  which rules do not provide, so `dialect.Compile` stays authoritative) → `dialect.Compile`
  + `prog.Init` on a `script` thread. **Never** freeze globals. **Never** `script.Exec`,
  `DefaultLimits`, `modules.Loader`, or `starlarkcap.Exec` — Exec freezes globals and is
  one-shot program-shaped; rules are long-lived and stateful.
- Callbacks via `script.Call` with the old budgets: `Limits{MaxSteps: 1_000_000,
  Timeout: 250ms}` per event/timer invocation (not script-services' 5M/10min defaults).
- Conversions via `values.FromGo/ToGo`; delete local go↔Starlark converters and the
  `init()` that mutates global resolver flags.
- Accepted language deltas, documented and tested: `while`/top-level control flow newly
  allowed; integral floats become ints; `[]byte` JSON-round-trips as base64; print hook
  loses position. `values.ToGo` int64 vs old float64 in signal `Where` matching gets a
  dedicated regression test (Stage 7).
- Known pre-existing gap carried knowingly: the `state` dict size cap is commented but
  unenforced today; a raw `*starlark.Dict` cannot enforce one.

Envcap handlers keep typed input schemas; the registry wrapper stamps the real job id;
lifecycle-changing handlers first start the durable op and stamp
`operation_id/job_id/env_id/generation/lease_epoch` distinctly into results.
Envcap imports no Starlark; consumers assemble the Starlark surface (`starlarkcap`).

Artifacts: the `Stage/Keep` port is owned by `envcap`; **environment-services has zero
artifact-services dependency**. Platform's artifactbridge implements the port over
artifact-services runs: `Deterministic:false`, job id = op id (+attempt suffix), core
claims the op **before** `Run` so racing executors cannot double-execute side effects,
failed-run partial outputs kept deliberately, symlink/dir/exec-mode preserved via
`craggy.envspec.modes` manifest metadata (artifact-services `KindLink` is not a filesystem
symlink) plus fixing the existing dir-skip in `copySourceToSink`. Round-trip test is a
Stage 9 and platform-stage gate. Evidence lives on the durable op; the artifact
occurrence is publication identity only.

## 7. Staged commits (every stage: build + vet + test + arch_test green on main)

### In this repo

- **S0** Ratify D1–D9; amend `plan.md`; scaffold go.mod/README/AGENTS/LICENSE/arch_test skeleton.
- **S1** `envspec` complete + golden JSON tests captured by running the **old** code now,
  covering every stream in old `collect.All()` (not only the five gates uses).
- **S2** Foundation ports: `internal/{adb, hostmon, xfer, signalcore, vault}`.
- **S3** `internal/{store, core, adapter, envtestfake}` with scriptable fake adapters
  (created **and** attached). Gate: plan §10 tests #1–7, #10 pass asserting DB rows.
- **S4** hostenv then docker adapters + owned collectors; `craggy.*` identities;
  Discover reports foreign `cclab.*`. Collector triage: port-all is default; dropping
  any of the 20 is a platform-visible change needing explicit sign-off here, not silence.
- **S5** `androiddev` + `androidemu` + android collectors + Supervisor-as-activity +
  MutationPolicy gate. Gate: §10 #8–9. **S5b** (droppable): `androidphys`.
- **S6** Services as activities. Gate: stale-generation `StopService` rejected.
- **S7** `internal/rules` on script-services. Gate: ported rule tests + regressions
  (state mutation across events, budget fault, `load()` rejected, int preservation, Where int64).
- **S8** `environments` assembly + `Observation`/`AndroidDevice` + `envtest` + demo e2e.
  Gate: `go doc` method list matches §3.2 exactly.
- **S9** `envcap` (read old `envcap/helpers.go` + `handlers_env.go` fully first — unread).
  Gate: fake-Artifacts tests; `Operation.ID` always set.
- **S10** Hardening: full arch matrix, `deadcode -test ./...` == 0, race tests,
  `docs/cutover.md`, orphan-warning test. Push; verify a clean module cache can
  `go get github.com/craggy-io/environment-services@<sha>` under consumers' GOPRIVATE.
  **No tag yet.**

### Freeze, then consumers (type identity makes lockstep unavoidable)

Platform passes concrete manager/hub/recorder values into gates' testbed, so platform
cannot compile against the new module while pinning old gates, or vice versa.

1. **FREEZE:** throwaway branches in `cclab-gates`, then `cclab-platform`, pin the S10
   pseudo-version; do the full cutover compiles; fix every facade miss **here**; re-pin.
   Only when both compile and test: tag `environment-services v0.1.0`.
2. **GATES:** repath imports; `androidtestbed.Config{Manager}` (hub/recorder trio deleted);
   move TLS/Cleartext challenges + PendingIntent script in-tree; alias-or-mirror per D5
   (drift tests deleted only where aliased); `testenv` → `environments.Open`;
   fake-collector tests → `FakeObservation`/goldens; golden-decode pre-cutover captures;
   inspect `res/*.star` for string-keyed payload fields. Tag.
3. **PLATFORM:** `openEnvironments` → `environments.Open`; delete readiness wrappers and
   back-calls; Handle renames; envjob fixes (release returns error and is joined; stop
   exit checked **and** Retain still runs; `RenewLease` ticker); overview rewired per D6;
   artifactbridge → Stage/Keep + symlink round-trip. Note: platform `core/scripts`
   reportedly fails typecheck today (pre-existing) — fix or exclude before using a
   whole-repo build as a gate. No `replace` directives anywhere; grep go.mod in CI.

### Clean-drain runbook (written as docs/cutover.md at S10; run at platform deploy)

Stop platform (old `ReleaseAll`) → inventory (`docker ps --filter label=cclab.manager`,
networks, adb serials/emulator ports 5554–5584, host workspaces, old DB non-terminal
rows) → force-remove leftovers **assuming leaks** (old code discards Destroy errors) →
archive old state/vault DBs (retained evidence/captures are **not** drained) → fresh
manager `Dir` (never reuse) → start new stack → `Warnings()` empty → one smoke provision
per kind + one gate experiment + one Ghidra acquire/stop. Rollback = old platform + old
Dir archive, after draining `craggy.*` resources via `Close(Drain)`.

## 8. Verification gates

plan.md §10 fault tests, all asserting DB rows (crash = reopen same Dir mid-op):
#1 concurrent identical provision → 1 op, 1 resource. #2 concurrent warm claims → 1 winner.
#3 crash after Create, before persist → Observe recovers. #4 release during provision,
late success → still releasing→released. #5 backend down in recovery → `unknown`, never
lost. #6 Destroy fails → op pending, visible, retried. #7 stale handle after reassign →
epoch rejection. #8 required collector dies → condition false + supervised restart.
#9 experiment hold → repair/reap denied + held. #10 recovered env released → reservation
sum returns. Plus: consumer FREEZE compiles, gates golden decode, platform ghidra
integration, overview JSON shape test.

## 9. Risks / unresolved (named, with owning stage)

- **R1** Publishability/GOPRIVATE for `craggy-io/environment-services` unverified — S10.
- **R2** Platform `core/scripts` typecheck failure (pre-existing, reported by audit) — platform stage.
- **R3** Unread bodies that may widen the facade: `envcap/helpers.go`, `envcap/handlers_env.go`,
  gates wave3/wave4/actor/canary, `res/*.star`, platform `NormalizeEnv` Data-key contract,
  parts of `android/{magisk,ramdisk,protocol}` and `collect` deep/net. Read at their stage, before FREEZE.
- **R4** `Transfer/Borrow` facade deletion assumes no platform production caller beyond
  envcap/services (untraced) — FREEZE compile is the check.
- **R5** `values.ToGo` int64 vs float64 in `Where` filtering — S7 test owns it.
- **R6** Golden sweep must cover every old stream, incl. logcat/fs payloads outside gates domains — S1.
- **R7** Physical-device exclusivity is single-host lease only. Stated non-goal.
- **R8** This session can write only in this repo; gates/platform stages must run as
  sessions in those repos (or with escalated access), in the order above.

## 10. Net effect

- Consumers: 12 imported packages → 3 production (+ `envtest` in tests).
- Public declarations: ~775 across old packages → the lists in §3 (roughly: envspec
  records, ~25 Manager methods, 2 facade interfaces, envcap's 9-ish declarations).
- Platform deletions enabled: assembly (~150 lines), readiness wrappers (~120), overview
  fan-in (~200 → JSON shaping only). Gates deletions enabled: hand assembly, mirrored
  payload structs + 4 drift tests (where aliased), hub/recorder plumbing in testbed.


