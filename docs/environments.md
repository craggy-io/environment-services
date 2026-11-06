# environments

`environments` is the one assembly and manager package. `Open` builds the
whole stack over a state directory — host sampler, credential vault, signal
hub and recorder, rule engine, runtime adapters, durable core — and `Manager`
is the entire consumer-facing lifecycle surface (23 methods, pinned by
`TestManagerMethodSet`).

Import path: `github.com/craggy-io/environment-services/environments`.

## Open and Config

```go
mgr, err := environments.Open(ctx, environments.Config{
    Dir:       "/var/lib/craggy/environments", // required; fresh per identity
    ManagerID: "mgr_platform",                 // stable across restarts
    AndroidSDK: "/opt/android-sdk",            // empty = no Android adapters
})
```

Only `Dir` is required. The host adapter always registers. Docker registers
when a `docker` binary (or `DOCKER_HOST`) is present, whether or not the
backend answers at Open — a stopped Docker stays startable on first need
(`EnsureProfileBackend`, or provisioning itself); Open never starts a
backend, it only seeds capacity when one answers. Android (emulator +
physical attach) registers only when `AndroidSDK` is set; `AVDHome` overrides
where emulator AVDs live (default `<Dir>/android/avd`), and
`FridaServerDir`/`FridaAutoDownload` control frida-server provisioning. Exactly one Manager may own a `Dir`
at a time (lock file). Set `ManagerID` when resources must be recognised as
this manager's across restarts — it is the value of the `craggy.manager`
ownership label; empty mints a fresh ID per Open. `MutationPolicy` gates
autonomous repair/reap (nil allows all); `Profiles` adds to the stock set
(`host-tools`, `linux-tools`, `android`, `android-play`,
`android-play-rooted`) and the user profiles (see [Profiles](#profiles));
`HostSampler` overrides host sampling in tests.

## Profiles

Open loads profiles once, through one foundation-kit `bundle.Load`, from
three owners:

| Owner | What it is |
| --- | --- |
| `craggy.environments.stock` | The built-in profiles (`StockProfiles`), a Go provider. |
| `craggy.environments.user` | The bundle at `<Dir>/profiles`, if that folder exists. |
| `craggy.environments.config` | `Config.Profiles`, a Go provider. |

The user bundle overrides the stock provider: a user profile with a stock
name replaces it, and new names are added. `Config.Profiles` overrides
nothing, so a name that stock or the user bundle already defines is an
Open error (`bundle.id_conflict`). Order never picks a winner.

`<Dir>/profiles` must be a bundle. Open doesn't create the folder; if it
is missing or empty there are no user profiles, and if it holds files but no
`bundle.yaml` Open fails (`bundle.manifest_missing`). The manifest's
`metadata.id` must be `craggy.environments.user`, and each profile is one
contribution of kind `environments.profile@1` whose file holds **one**
JSON `envspec.Profile` (not a list). Its `name` must equal the
contribution `id`. Unknown fields are errors, and each file is checked as
`RegisterProfiles` would check it:

```yaml
# <Dir>/profiles/bundle.yaml
apiVersion: craggy.io/bundle/v1
metadata:
  id: craggy.environments.user
  version: 0.1.0
contributions:
  - id: busybox
    kind: environments.profile@1
    path: busybox.json
```

```json
{"name": "busybox", "kind": "container", "image": "busybox:latest", "allow_images": ["busybox:*"]}
```

Profile names are bundle IDs, so they use letters, digits, `.`, `_` and
`-`, and start and end with a letter or digit. Profiles are read only at
Open; edit the bundle and reopen the manager to pick up changes.
`RegisterProfiles` still registers profiles at run time from Go.

## Provision, use, release

```go
h, err := mgr.Provision(ctx, owner, envspec.Spec{Profile: "linux-tools"},
    environments.WithRequestKey("job-42"),
    environments.OnProgress(func(p envspec.Progress) { /* stage updates */ }))
out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"echo", "hi"}})
err = h.Push(ctx, envspec.MemSource(map[string][]byte{"in.txt": data}), "/work")
sink := envspec.NewMemSink()
err = h.Pull(ctx, "/work", sink)
err = h.Release(ctx) // or mgr.Release(ctx, h.ID())
```

Provisioning is durable: the environment row, operation and lease are
persisted before any runtime action, so a crash mid-provision resumes instead
of leaking. The handle is issued last, after admission, identity
verification, credential injection and required-signal barriers. Release is
equally durable — it completes only after the runtime resource is verified
gone (or disconnected, for attached devices); failures stay pending and
retry across restarts.

Holders keep a lease alive only by calling `RenewLease(handle)`; using a
handle never renews it implicitly. Long-running holders must run their own
renewal ticker.

## Reading state

- `Get(id)` re-acquires a handle — it validates the live lease, so a handle
  obtained before reassignment stays permanently stale.
- `List(Filter)`, `mgr` events: `Events`/`EventsSince`/`RecentEvents` page
  the durable lifecycle log (`envspec.LifecycleEvent`) by sequence cursor.
- `Profiles`, `Profile`, `ProfileStatus` (`Ready`/`NotReady` says why not),
  `StockProfiles`, `EnsureProfileBackend`, `BuildDigest`, `Drivers`,
  `HostSnapshot`, `ManagerID`.
- `RedactString` scrubs known secrets from a string (the vault redactor).

There is deliberately no overview/aggregation struct: render from
`Profiles`/`List`/`EventsSince`/`HostSnapshot` (plus envcap JSON where a
capability surface is in play).

## Android

`Android(envID)` returns an `AndroidDevice` for an Android environment this
manager owns: `Shell`/`RootShell` (device-shell strings, unlike argv-only
`Handle.Exec`), `Install`/`Uninstall`/`StartApp`/`ForceStop`,
`Tap`/`Swipe`/`KeyEvent`/`Text`, `PackageSnapshot`,
`ApplyPackageSequenceAndAction`, `BackupToLocalTransport`, `Logcat`,
`Screenshot`. Typing a vault secret into a device is intentionally **not**
on this interface; it exists only inside the capability surface, where the
secret never leaves the vault.

`StartTLSChallenge(ctx, envspec.TLSChallengeOptions)` and
`StartCleartextChallenge(ctx, envspec.CleartextChallengeOptions)` start a
host-owned network peer for the app under test and return a
`Challenge[R]` (`URL`, `Receipt`, `Close`) whose receipt is observed by the
host, independently of what the app reports:

- TLS: a leaf issued for `ExpectedHost` by an ephemeral, untrusted CA, so
  the only validation failure is `envspec.TLSFailureUntrustedChain`.
  `envspec.TLSChallengeReceipt` pins the leaf (`CertificateSHA256`,
  `CertificateDER`) and lists the `Sessions` that completed a handshake — a
  correctly validating or pinning client completes none.
- Cleartext: HTTP on an unguessable path. `envspec.CleartextChallengeReceipt`
  lists every request by path digest and whether it matched; the raw path
  is never retained.

The device reaches the peer at `envspec.ChallengeHostAlias` (`10.0.2.2`, the
emulator's alias for the host loopback, where the server then listens
loopback-only). A physical device or another bridge needs an explicit
`AdvertiseHost`, which binds every interface. The lease owns each challenge:
release, expiry, warm reset and destroy close it, as do `Close` and
cancelling the start `ctx`; `Receipt` stays readable afterwards.

## Services

`AcquireService(owner, ServiceRequest)` returns an `envspec.ServiceLease`
over a shared managed service environment; `StopService(id, generation)` is
generation-fenced, so a stale stop can never kill a newer incarnation.
`Manager` satisfies `envspec.ServiceProvider`.

## Observation and capabilities

`Observation()` returns the signal surface — see [signals](signals.md).
`NewEnvCapDeps(mgr)` adapts the manager to the capability surface — see
[capabilities](capabilities.md). Neither widens `Manager` itself.

## Shutdown

`Close(ctx, ShutdownDrain)` releases everything this manager owns;
`Close(ctx, ShutdownDisconnect)` detaches and leaves durable intent for the
next Open over the same `Dir` to resume (pending operations, warm pools).
There is no retain mode. `Close` is idempotent.

Foreign resources — other managers', other tooling's, leftovers — are never
adopted. Discovery skips them, and this package creates and recognises only
`craggy.*` identities.
