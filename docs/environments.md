# environments

`environments` is the one assembly and manager package. `Open` builds the
whole stack over a state directory — host sampler, secret redactor, signal
hub and recorder, rule engine, runtime adapters, durable core — and `Manager`
is the entire consumer-facing lifecycle surface (24 methods, pinned by
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
`HostSampler` overrides host sampling in tests. `Credentials` is the port to
the application's credential store (see [Credentials](#credentials)); nil
makes a spec with `Creds`, and typing a secret, unavailable.

## Credentials

The manager keeps no credential. The application implements
`environments.Credentials` over its own store and sets it as
`Config.Credentials`:

```go
type Credentials interface {
	Hold(ctx context.Context, req CredentialRequest) (CredentialLease, error)
	Redeem(ctx context.Context, lease, holder string) (HeldCredential, error)
	RedeemHolder(ctx context.Context, holder string) ([]HeldCredential, error)
	Drop(ctx context.Context, holders []string) error
}
```

A holder is `env:<environment id>`. Every lease names a use, one of three
the application declares to its store, and the delivery of an
`envspec.CredRequest` decides which:

| Use (`envspec`) | Delivery | What it does |
| --- | --- | --- |
| `UseAndroidAccount` `env.android.account` | `account`, or `type` with `use` set | Signs an Android device or emulator in to the account. |
| `UseLogin` `env.login` | `type` (the default), `env.android.type-secret` | Typed into a sign-in form or field inside an environment. |
| `UseInject` `env.inject` | `env:NAME`, `file:/path` | Handed to the environment's processes as a variable or a file, where whoever runs commands there can read it. |

Provisioning a spec with `Creds` calls `Hold` for each request, then
`Redeem` and delivers; a `type` request is only held, and redeemed when it
is typed. The use of a redeemed lease is checked against the delivery every
time, so a lease taken for typing is never written into a variable or a
file, and the reverse. Each redeemed secret is taught to the redactor for
as long as its holder lives.

The port is called with the context of the work it is part of. The
manager's own background work (the reaper releasing an expired
environment, recovery at Open) belongs to no call the application could
act under, so the port returns `ErrNoCall` there and the manager defers:

- **Ending leases.** Release queues the holder in the state database, then
  calls `Drop`; the holder leaves the queue only when `Drop` succeeds.
- **After a restart.** Nothing is redeemed at Open. A reopened environment
  gets its variables and files back, and the redactor its secrets, from
  `RedeemHolder`.

Both happen in `Manager.SyncCredentials(ctx)`, which the application calls
with the context of each call it serves, before the call's own work. It is
cheap when nothing waits. A provision the manager resumes by itself after a
crash can't lease, so one with `Creds` fails and is released: provision
again.

Port errors pass through to the caller. Wrap `envspec.ErrNotFound`,
`environments.ErrCredentialUnavailable` (the store's limits refuse the lease
for now; envcap answers `unavailable`) or `environments.ErrCredentialDenied`
(the credential may not be put to that use; `denied`).

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
- `RedactString` scrubs the secrets the manager is delivering from a string.

There is deliberately no overview/aggregation struct: render from
`Profiles`/`List`/`EventsSince`/`HostSnapshot` (plus envcap JSON where a
capability surface is in play).

## Android

`Android(envID)` returns an `AndroidDevice` for an Android environment this
manager owns: `Shell`/`RootShell` (device-shell strings, unlike argv-only
`Handle.Exec`), `Install`/`Uninstall`/`StartApp`/`ForceStop`,
`Tap`/`Swipe`/`KeyEvent`/`Text`, `PackageSnapshot`,
`ApplyPackageSequenceAndAction`, `BackupToLocalTransport`, `Logcat`,
`Screenshot`. Typing a leased secret into a device is intentionally **not**
on this interface; it exists only inside the capability surface, where the
manager redeems the lease and the value crosses no capability.

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
For a host-side HTTP probe, set `ServiceRequest.Environment.PublishedTCPPort`
to the container's listening TCP port and select an `open` or `egress`
network. The Docker adapter publishes an ephemeral loopback-only host port;
borrowers read its address from `ServiceLease.Info().Endpoints["tcp"]`.

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
