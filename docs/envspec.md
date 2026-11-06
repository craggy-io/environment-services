# envspec

`envspec` is the standard-library-only record package: everything a consumer
needs to *declare* environments and *name* the data that comes back, with no
driver, SQLite or Docker client pulled in transitively. That property is
load-bearing — consumer domain packages alias these types — and is enforced by
`TestEnvSpecStdlibOnly` in [arch_test.go](../arch_test.go).

Import path: `github.com/craggy-io/environment-services/envspec`.

## What lives here

| Area | Types |
| --- | --- |
| Requests | `Spec`, `Profile`, `Build`, `Resources`, `Kind` (`container`, `android`, `host`), `Network`, `AndroidOptions`, `HostOptions`, `CredRequest`, `SignalRequest`, `Owner` |
| Holder surface | `Handle` (`ID`, `Info`, `Exec`, `Push`, `Pull`, `Release`), `Cmd{Argv, Env, Timeout}`, `Source`/`Sink` with `MemSource`/`NewMemSink`, `Entry`, `TransferStats`, `Progress` (+ `Throttled`) |
| Read models | `Info` (with `Conditions []Condition`), `LifecycleEvent`, `HostSnapshot`, `HostCapacity`, `PoolCapacity`, `ProfileStatus`, `State` |
| Signals | `SignalEvent`, `Selector`, `Filter`/`Predicate`/`Op`, `Quality`/`Gap`, `Coverage`, `RangeMeta`, `StreamState`, stream-name constants (`StreamAndroidCrash = "android.crash@2"`, …) and every stream payload type (`CrashEvent`, `StateEvent`, `FridaData`, `FlowData`, …) |
| Android receipts | `PackageSnapshot`, `PackageMutation`/`PackageAction` (+ receipts), `BackupReceipt`, `TLSChallengeOptions`/`TLSChallengeReceipt`/`TLSSession`, `CleartextChallengeOptions`/`CleartextChallengeReceipt`/`CleartextRequest`, `ChallengeHostAlias`, `TLSFailureUntrustedChain`, `PendingIntentProtocol` |
| Services | `ServiceSpec`, `ServiceRequest`, `ServiceInfo`, `ServiceProvider`, `ServiceLease` |
| Policy + ids | `MutationPolicy` (the experiment-hold port), `NewID`/`HasPrefix`, sentinels `ErrNotFound`, `ErrInvalid`, `ErrNotReady`, `ErrConflict`, `ErrBackupRejected`, `ErrBackupRecipientEmpty` |

## Rules

- **Records only.** No behaviour beyond tiny helpers (`Throttled`, `Entry`
  predicates). Matching, storage and lifecycle live in `internal/`.
- **JSON is frozen.** Stream payloads and receipts are a wire contract shared
  with consumers that persist captures: field names and stream versions
  (`android.crash@2`, `net.flows@1`, …) never change. Golden tests in
  `envspec/testdata` pin every shape; a golden diff is a breaking change, not
  a test update.
- **Handles are lease-aware values.** A `Handle` names one holder's lease on
  one generation of an environment. After release or reassignment the handle
  is permanently stale and its operations fail; there is no way to revive it.
- **`Spec.Key` (or `environments.WithRequestKey`) makes provisioning
  idempotent** per owner: the same key with the same merged spec resolves to
  the same durable operation, even across a manager restart; the same key
  with a different spec is `ErrConflict`.
- **Required signals block readiness.** `SignalRequest.Required` pins block
  the provision barrier until the collector acknowledges; the default is
  best-effort.

See [environments](environments.md) for the manager that acts on these
records and [signals](signals.md) for the observation model.
