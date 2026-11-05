<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/craggy-wordmark-on-dark.svg">
  <img src="docs/assets/craggy-wordmark-on-light.svg" alt="Craggy" width="300">
</picture>

# Environment Services

A durable lifecycle manager for test environments: containers, Android
emulators and physical devices, and host processes. One core owns identity,
leases, admission, durable operations, readiness conditions and cleanup;
runtime adapters own the mechanisms. Observation (logs, packets, Frida,
device state) plugs in as supervised activities with receipt-grade records.

The module is `github.com/craggy-io/environment-services`. Consumers see three
production packages (`envspec`, `environments`, `envcap`) plus one test-only
package (`envtest`); everything else is `internal/`. This is a full cutover
with no legacy compatibility: fresh state directories and `craggy.*`
identities only, or nothing. Deploying over a previous stack is a clean
drain — see [docs/cutover.md](docs/cutover.md).

## Features

- **Durable lifecycle:** [provision, release, reset and attach are persisted
  operations](docs/environments.md) recorded before any runtime action, so a
  crash mid-flight resumes instead of leaking; release completes only on
  verified absence. Idempotent provisioning by request key, lease-epoch
  fenced handles, admission recomputed from durable rows.
- **Four runtimes, one core:** host processes, Docker containers, Android
  emulators and attached physical Android devices (attach/disconnect only,
  never create/destroy), behind one adapter contract with explicit
  present/absent/unknown observation.
- **Observation as evidence:** [versioned signal streams](docs/signals.md)
  (logcat, packets/flows/DNS/TLS/HTTP, binder, IPC, Frida, crashes, fs
  changes) with pins, coverage answers and loss accounting, so "nothing
  happened" is never confused with "nobody was watching".
- **Signal rules:** [stateful, budgeted Starlark rules](docs/rules.md) on
  script-services that emit derived signals, raise triggers and pin captures
  — and can never act on the world.
- **Capability surface:** [38 registry capabilities](docs/capabilities.md)
  over the same manager, with durable operation identities distinct from
  registry jobs and an injected artifact Stage/Keep port (this module has no
  artifact-services dependency).
- **Secrets stay secret:** a credential vault with leases, redaction before
  recording, and secret-typing that never lets a value leave the vault.
- **Small public surface, enforced layering:** `envspec` is
  standard-library-only (safe for domain packages to alias), and
  `arch_test.go` pins the whole import matrix.

## Package architecture

Read top to bottom: each layer imports only the layers below it (enforced by
`TestLayering` in [arch_test.go](arch_test.go)).

| Package | Responsibilities | Imports (in this module) | Guide |
| --- | --- | --- | --- |
| `envspec` | Records and narrow interfaces: specs, profiles, handles, signal payloads, Android receipts. | none (stdlib only) | [envspec](docs/envspec.md) |
| `envcap` | The 38 registry capabilities, operation/evidence records, the artifact Stage/Keep port. | envspec | [capabilities](docs/capabilities.md) |
| `environments` | The one assembly root: `Open` + `Manager`, `Observation`, `AndroidDevice`, envcap adapter. | envspec, envcap, internal/* | [environments](docs/environments.md) |
| `envtest` | Test-only fakes (`FakeObservation`, `FakeServiceProvider`). Never for production. | envspec, environments | [signals](docs/signals.md#in-tests) |
| `examples/demo` | Assembly smoke test: provision → push/exec/pull → capabilities → drain. | envspec, environments, envcap | — |
| `internal/core` + `internal/store` | Durable lifecycle, leases, admission, operations, activities. | envspec, adapter, hostmon, signalcore, vault, xfer | [design](docs/design.md) |
| `internal/adapters/{docker,hostenv,androidemu,androidphys}` | Runtime mechanisms and their owned collectors. | envspec, adapter, signalcore (+ own tech) | — |
| `internal/androiddev` + `internal/adb` | Device-over-serial surface, Frida, component supervisor; the adb transport. | envspec, adapter, signalcore | — |
| `internal/signalcore` | Hub, recorder, ranges, selector/filter matching. | envspec | [signals](docs/signals.md) |
| `internal/rules` | Starlark rule engine (the only script-services/Starlark importer). | envspec, signalcore | [rules](docs/rules.md) |
| `internal/vault`, `internal/hostmon`, `internal/xfer` | Credential leasing/redaction (own DB); host sampler; file-tree transfer (+ `cmd/xfer-agent`). | envspec | — |

External confinement: `capability-services` only in `envcap` (+ demo),
`script-services`/`go.starlark.net` only in `internal/rules`, moby/gopacket
only in the docker adapter, and no artifact-services import anywhere.
Design rationale: [docs/plan.md](docs/plan.md) as amended by
[docs/execution-plan.md](docs/execution-plan.md); see also
[AGENTS.md](AGENTS.md).

## Quick start

Open a manager over a fresh directory and see what it starts with. From
[`ExampleOpen`](environments/example_test.go), compiled and checked by
`go test`:

```go
mgr, err := environments.Open(ctx, environments.Config{Dir: dir, ManagerID: "mgr_example"})
if err != nil {
	panic(err)
}
defer mgr.Close(ctx, environments.ShutdownDisconnect)
// mgr.StockProfiles() → [android android-play android-play-rooted host-tools linux-tools]
// mgr.Drivers()       → hostenv always; docker/android by presence and config
// mgr.ProfileStatus("host-tools").Ready → true
```

Then provision, work, release:

```go
h, err := mgr.Provision(ctx, owner, envspec.Spec{Profile: "host-tools", Key: "demo-host"})
err = h.Push(ctx, envspec.MemSource(map[string][]byte{"hello.txt": []byte("hello\n")}), "/work/in")
out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"cat", "in/hello.txt"}})
sink := envspec.NewMemSink()
err = h.Pull(ctx, "/work", sink)
err = mgr.Release(ctx, h.ID())
```

Run the whole path end-to-end (manager, host environment, push/exec/pull,
capability registration and invocation, drain):

```sh
go run ./examples/demo
```

The host adapter always registers; Docker needs a reachable daemon and
Android needs `Config.AndroidSDK` set. Set a stable `Config.ManagerID` for
any manager whose resources must be recognised across restarts, and call
`RenewLease` from a ticker for long-lived holds — using a handle never
renews its lease implicitly.
