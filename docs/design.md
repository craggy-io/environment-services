# Design notes

Distilled from [plan.md](plan.md) (the design authority) as amended by the
governing decisions in [execution-plan.md](execution-plan.md) §0. Where the
two disagree, the execution plan wins.

## The separation

One durable **lifecycle core** owns identity, desired state, leases,
admission, operations, readiness conditions and cleanup. **Adapters** own
runtime mechanisms (Docker, host processes, Android emulator, physical
devices) and report facts; the core decides the next action and is the only
writer of durable state. **Activities** — probes, collectors, components,
managed services — run beside an environment under one scheduler with
backoff and acknowledgement. Platform concerns (callers → owners, artifact
destinations, presentation) stay outside this module.

Lifecycle, leasing, health and collection are separate dimensions, not one
state enum: desired disposition, lifecycle phase, observed presence
(present/absent/**unknown** — a backend that cannot answer never proves
absence), lease, per-name conditions (True/False/Unknown with reason,
observation time and per-name freshness computed at read) and activity
states are stored and reported separately.

## Durability in one paragraph

One SQLite file holds five tables plus the event log (`envs`, `operations`,
`leases`+`borrows`, `conditions`, `activities`, `events`); the vault keeps
its own file. Every state change is a typed conditional transition
(`… WHERE id=? AND revision=?`, else `ErrConflict`), and a failed durable
write closes admission rather than proceeding on volatile state. Provision
persists env + operation + lease **before** dispatching; resources are
named deterministically from the env ID (`craggy-<envID>`, docker labels
`craggy.env`/`craggy.manager`/`craggy.profile`) so a crash between the
external action and its record is recoverable by rediscovery. Handles are
issued last, after admission → create/attach → identity verify → credentials
→ required activities acked. Release records intent, then stops activities,
releases vault leases, destroys or disconnects, and completes only on
verified absence — otherwise the operation stays pending with backoff and
resumes across restarts. Idempotency is a unique `(owner_scope,
request_key)` operation row whose request digest must match on retry.
Admission is a recomputed sum over durable rows, so capacity cannot leak
through lost callbacks.

Durable operations exist only for `provision | release | reset | attach`.
Exec, Push, Pull and device actions stay ephemeral, fenced per call by a
generation+epoch handle check. There is no second job system: registry jobs
are a projection of durable operations.

## Adapters

The contract (`internal/adapter`) mandates `Describe/Validate/Observe/
Discover` and makes everything else optional (`Creator`, `Attacher`,
`Executor`, `Transferrer`, `Snapshotter`, `Preparer`, `ImageBuilder`,
`CredInjector`, `FSLister`, …), so a physical phone needs no `Create` and a
container no `Attach`. Created resources are destroyed on release; attached
resources are only disconnected. Each adapter owns its signal collectors,
written against typed ports from its own adapter instance — no cross-adapter
concrete types, no stringly-typed escape hatches. Discovery is scoped to
this manager's identities; anything foreign is skipped, never adopted.

## Signals and rules

Collectors run once and return; the hub routes and records but owns no
lifecycle — pins are core activities (see [signals](signals.md)). Rules are
stateful, budgeted Starlark observers on script-services that emit derived
signals and triggers but can never act on the world (see
[rules](rules.md)). Autonomous repair (e.g. the Android component
supervisor: capture-before-repair, suspect limits) passes through the
`MutationPolicy` port, so an experiment hold can freeze repairs while
observation continues. Holder lease renewal, target probes and job progress
are three different signals and never substitute for each other.

## Identity and non-goals

Full cutover, no legacy: fresh state directories, `craggy.*` identities
only, no adoption, migration, compatibility projection or shims, and
obsolete code is deleted rather than deprecated. Shutdown is
`Drain | Disconnect` only (no retain). Physical-device exclusivity is a
single-host lease. The module has no artifact-services dependency: the
Stage/Keep publication port is owned by `envcap` and implemented by hosts
(see [capabilities](capabilities.md)).
