# Capabilities (envcap)

`envcap` projects a manager into a
[`capability-services`](https://github.com/craggy-io/capability-services)
registry: `Capabilities(Deps)` returns `registry.Capability` values (33,
grouped by `Groups()`), ready for `Registry.Add`/`AddGroup`, so every
registry projection (HTTP, MCP, tools, Starlark) sees the same surface.

Import path: `github.com/craggy-io/environment-services/envcap`.

**Trust boundary:** envcap sits *inside* it. `Deps.Manager` deliberately
includes operations the consumer facade hides — borrows, by-ID
exec/transfer, secret typing — because the host
assembling envcap has decided its callers may do these things. Map callers
to owners only through `Deps.Owner` (trusted invocation context), never
from capability arguments. envcap does not import `environments`; wire it
with `environments.NewEnvCapDeps(mgr)`, which satisfies `envcap.Manager`
structurally (compile-time asserted in `environments`). envcap imports no
Starlark: hosts assemble a Starlark surface themselves
(capability-services' `starlarkcap`).

## The 33 capabilities

| Group | Capabilities |
| --- | --- |
| `env` | `env.status`, `env.profiles.list`, `env.provision`, `env.attach`, `env.list`, `env.get`, `env.release`, `env.keepalive`, `env.exec`, `env.fs.list`, `env.fs.stat`, `env.push`, `env.pull`, `env.snapshot.save`, `env.snapshot.load`, `env.snapshot.list` |
| `env.android` | `env.android.install`, `env.android.screenshot`, `env.android.input`, `env.android.type-secret`, `env.android.frida.hook` |
| `host` | `host.status` |
| `signals` | `signals.streams.list`, `signals.query`, `signals.tail`, `signals.pin`, `signals.unpin`, `signals.capture` |
| `rules` | `rules.load`, `rules.unload`, `rules.list`, `rules.test`, `rules.triggers.wait` |

Every capability declares a strict JSON-Schema input (closed objects) and a
description that says when to choose it. Owners see only their own
environments; a foreign environment is indistinguishable from a gone one.
Errors carry stable codes, recovery hints, and — for unavailability — a
reason (`no_capacity`, `backend_down`, …) so agents can decide whether to
retry. Long operations report `envspec.Progress` verbatim as registry job
progress.

For a USB device already authorised for ADB debugging, use
`env.attach {"serial":"DEVICE_SERIAL","key":"optional-retry-key"}`.
The `android-physical` profile uses an unrooted device configuration.
The attach verifies identity and claims an exclusive local lease; `env.release`
disconnects the lab without wiping the device. Device commands and app
experiments can still change the device, so callers must choose those
actions deliberately.
For an already rooted device requiring instrumentation, explicitly use
`env.attach {"serial":"DEVICE_SERIAL","root":"magisk","frida":true}`.
This requests a Magisk root shell and may push and start a managed
frida-server; `frida:true` without an explicit root mode is rejected.
The physical adapter reads `ro.product.cpu.abi` so it selects the matching
Frida binary on arm64 devices.

## Operations and artifacts

Lifecycle handlers (`env.provision`, `env.attach`, `env.release`) start the durable
lifecycle operation first, then stamp `operation_id`, `job_id`, `env`,
`generation` and `lease_epoch` **distinctly** into results: the registry job
is a projection (after a restart the job is lost; the operation resumes, and
a retry mints a new job bound to the same operation). `Operation.ID` is
always set; `Operation.JobID` comes from the registry JobID, never the
owner's job; `WithParentOperation` threads a parent operation through ctx.

Artifact publication is a port owned here — this module has **no
artifact-services dependency**. The host implements `Artifacts.Run` once
over its artifact store (`nil` degrades artifact paths to inline/count-only
results):

- `Stage` copies a tree into the run; `Keep` publishes it. Outputs kept
  before a later failure stay kept: `Run` returns them with an error
  wrapping `PartialOutputs` (see `PartialOutputsOf`).
- Directory, symlink and executable modes survive via a
  `craggy.envspec.modes` manifest prefixed into staged trees
  (`EncodeModes`/`DecodeModes`), because artifact link kinds are not
  filesystem symlinks.
- Evidence (`envcap.Evidence`) rides on the durable operation; the artifact
  occurrence is publication identity only.

`env.android.type-secret` is the one secret-moving capability: it types a
leased credential field into a device, and the secret value never appears in
inputs, outputs or logs. envcap serves no credential store: the lease id
comes from the application's own (`core.credentials.lease` in the harness),
taken for the use `env.login` or `env.android.account` with the holder
`env:<environment id>`. The manager redeems it through its credentials port
([environments](environments.md#credentials)) and refuses a lease taken for
`env.inject` or held by anything else. `env.provision`'s `creds` lease and
deliver the same way; `env.status` reports no credential leases, which are
the store's to list.
