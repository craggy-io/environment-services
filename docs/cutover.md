# Cutover runbook

This is a **full cutover with a clean drain**: no state, labels or resources
carry over from the previous environments stack. The new module
(`github.com/craggy-io/environment-services`) creates and recognises only
`craggy.*` identities and starts from a fresh state directory. Old-stack
resources are drained and removed by hand first; anything left behind is
foreign and will never be adopted (there is no adoption, migration, or
warnings API — the inventory below is the operator's responsibility).

Programme context and ordering rationale: [execution-plan.md](execution-plan.md)
§7. Platform passes concrete manager values into gates' testbed, so the three
repos cut over in lockstep: freeze here, then gates, then platform.

## Phase 1 — FREEZE (in this repo, before any tag)

1. Land all stages with the full gate green:
   `go build ./... && go vet ./... && go test ./... -count=1`
   (plus `arch_test.go`'s layering matrix, which runs inside `go test`).
2. Push `main` (**no tag yet**) and record the commit SHA; the consumer pins
   are pseudo-versions of that SHA.
3. Verify publishability from a clean module cache under the consumers'
   `GOPRIVATE` settings (the repo is served from
   `https://github.com/craggy-io/environment-services.git`):
   - `go list -m -versions github.com/craggy-io/environment-services`
   - in a throwaway module with an empty `GOMODCACHE`:
     `go get github.com/craggy-io/environment-services@<sha>` and build a
     file importing `envspec` and `environments`.
4. In throwaway branches of **cclab-gates**, then **cclab-platform**, pin the
   pseudo-version and do full cutover compiles (phases 2–3 below). Fix every
   facade miss in **this** repo, push, and re-pin. Do not work around a
   facade miss inside a consumer.
5. Only when both consumers compile and their tests pass: tag
   `environment-services v0.1.0`, then re-pin both consumers to `v0.1.0`.

No `replace` directives at any point, in any repo (grep `go.mod` before
merging each branch).

## Phase 2 — Gates session (run in the cclab-gates repo)

1. Repath all environment imports to
   `github.com/craggy-io/environment-services/{envspec,environments,envcap}`
   (`envtest` in tests only). Delete imports of the old packages outright —
   no aliases of old packages/types.
2. `androidtestbed.Config` takes the new `*environments.Manager` (plus what
   it derives from it); delete the hub/recorder plumbing — observation comes
   from `Manager.Observation()`.
3. Move the TLS/Cleartext challenge code and the PendingIntent observer
   script into gates (they left this module; only the
   `android-pendingintent@1` protocol constant and receipts remain, in
   `envspec`).
4. Per stream domain, either alias the `envspec` payload types or keep local
   mirrors (your choice per D5); delete drift tests only where aliased.
5. Replace `testenv` hand-assembly with `environments.Open`; replace
   fake-collector tests with `envtest.FakeObservation` plus JSON goldens.
6. Golden-decode pre-cutover captures against the new `envspec` types; every
   stream must decode unchanged (JSON is frozen). Inspect `res/*.star` for
   string-keyed payload fields that must keep matching.
7. Gate: module builds, vets and tests green against the pinned version.
   Then tag gates.

## Phase 3 — Platform session (run in the cclab-platform repo)

1. Replace `openEnvironments` with `environments.Open(ctx, Config{Dir:
   <fresh dir>, ManagerID: <stable id>, AndroidSDK: ...})`. Delete the
   wiring that derived a runtime manager label from the durable store ID.
2. Delete the profile-readiness wrappers; use `Manager.ProfileStatus`.
   Rewire the overview from `Profiles`/`List`/`EventsSince`/`HostSnapshot`
   plus envcap JSON (no overview struct exists).
3. Apply the Handle renames: holders use `envspec.Handle`
   (`Exec`/`Push`/`Pull`/`Release`); delete uses of the old handle method
   names in `core/ghidra`, `core/envjob`, `application/ghidra.go` and test
   fakes.
4. `envjob`: release returns an error and must be joined (no fire-and-forget
   cleanup); check the stop command's exit status **and** still run the
   retain/output path on failure; own job liveness with a ticker calling
   `Manager.RenewLease(handle)` — using a handle no longer renews its lease.
5. `artifactbridge`: implement the envcap Stage/Keep port
   (`envcap.Artifacts.Run`) over artifact-services runs —
   `Deterministic:false`, run job id = operation id (+attempt suffix),
   claim the operation through the lifecycle before executing, keep
   failed-run partial outputs deliberately, and round-trip the
   `craggy.envspec.modes` manifest so symlink/dir/exec modes survive.
   Gate on a symlink round-trip test.
6. Register `envcap.Capabilities(envcap.Deps{Manager:
   environments.NewEnvCapDeps(mgr), Artifacts: <bridge>, Owner: <trusted
   ctx mapping>})` and `envcap.Groups()`.
7. Note: platform `core/scripts` was reported failing typecheck before this
   programme (pre-existing). Fix or explicitly exclude it before using a
   whole-repo build as a gate; do not let it mask cutover breakage.
8. Gate: builds, vets, tests green; Ghidra acquire/stop integration passes
   (service generation, borrower lease and transaction lock stay distinct);
   overview JSON shape test passes.

## Phase 4 — Clean drain at deploy (operator, on the host)

Assume the old stack leaks: it discards some Destroy errors, so inventory
and force-remove rather than trusting its release path.

1. Stop platform (the old stack's `ReleaseAll` runs on shutdown). Wait for
   the process to exit.
2. Inventory everything the old stack may have left (old-stack identities
   shown for this step only — the new stack neither creates nor recognises
   them):
   - `docker ps -a --filter label=cclab.manager` and
     `docker network ls` (old sidecar networks).
   - `adb devices`; emulator console ports 5554–5584
     (`adb -s emulator-<port> emu kill` for any alive).
   - Host workspaces under the old state directory; stray agent processes.
   - Old state DB rows in non-terminal states, for the record.
3. Force-remove the leftovers: `docker rm -f` the listed containers,
   `docker network rm` their networks, kill leftover emulators and remove
   their AVD run state, delete orphaned host workspace dirs.
4. Archive (do not delete) the old state and vault DBs. Retained evidence
   and captures are **not** drained — they stay where the artifact/evidence
   stack keeps them and remain readable (signal JSON is frozen; decode them
   with the new `envspec` types).
5. Point platform at a **fresh** manager `Dir` (never reuse the old one)
   with a stable `ManagerID`, and start the new stack.
6. Smoke checks, in order:
   - `docker ps -a --filter label=craggy.manager` shows only resources the
     new manager creates from here on.
   - One smoke provision per kind in use (host, container, Android) with
     exec + push/pull, then release and confirm the runtime resource is
     gone.
   - One gate experiment end-to-end (signals pinned, captures readable).
   - One Ghidra service acquire/borrow/stop.
7. Done. From here the new manager's own recovery owns its `Dir`: restarts
   resume durable operations; `Close(Drain)` empties it deliberately.

## Rollback

Rollback is the old platform build against the archived old `Dir` — but
only after draining the **new** stack's resources: stop the new platform
with `Close(ShutdownDrain)`, verify
`docker ps -a --filter label=craggy.manager` is empty and no new-stack
emulators remain, then restore the old binary and old `Dir` archive. Never
run both stacks against the same Docker daemon expecting either to clean up
after the other: each treats the other's resources as foreign.
