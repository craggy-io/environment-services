# Signals and observation

Signals are the recorded runtime evidence of environments: logcat, packet
flows, crashes, Frida events, filesystem changes, resource samples. Each
adapter owns its collectors (there is no generic collector package);
`internal/signalcore` is only the hub, recorder, range and filter machinery.
Consumers see signals through `environments.Observation`, the
`signals.*` capabilities, and the `envspec` records below.

## Model

- A **stream** is a versioned name with a fixed payload type, e.g.
  `android.crash@2` → `envspec.CrashEvent`, `net.flows@1` → `FlowData`,
  `host.metrics@1` → `HostSnapshot`. Constants live in `envspec`
  (`StreamAndroidCrash`, …). Versions and payload JSON are frozen.
- A **source** is an environment ID, or `host` for host streams.
- A **collector** is one adapter-owned mechanism that produces one or more
  streams for one source under one params value. Params are part of
  collector identity (digested): the same stream with different params is a
  different configured run.
- An **event** (`envspec.SignalEvent`) carries stream, source, collector
  instance, per-instance sequence, time and payload. Signals are lossy by
  design; sequence gaps and drop counters are reported in
  `envspec.Quality`, never hidden.

Lifecycle events are **not** signals: provisioning/release history is the
durable, lossless `Manager.Events*` log.

## Pins keep collectors running

Nothing runs a collector implicitly. A **pin** is the durable statement
"run this collector" and is owned by the lifecycle core as an activity:

- `Spec.Signals` pins collectors from provision; `Required` pins block
  readiness until the collector acknowledges it is running.
- `Observation.Pin(stream, source, params)` starts one at runtime and
  returns an idempotent unpin func; it fails synchronously when no collector
  provides the stream, the source is not running, or the collector cannot
  run there. `PinFresh` restarts a run (after resetting a process whose
  observer cannot survive the reset).
- Queries and tails never start anything. Pin first, then read.

Collectors run once and return; the core's activity reconciler owns retry
with backoff, and run history (start, stop, failure) backs coverage answers.

## Reading

- `Observation.Query(sel, from, to)` reads recorded events with their
  `Quality`. Selectors (`envspec.Selector`) match collector instances,
  stream/source globs (`net.*@1`, `env_*`), exact params, and `Where`
  predicates over payload fields (`envspec.Filter`).
- `Observation.ConfiguredState(stream, source, params)` — is exactly this
  configured collector running now? `Running()` lists live instances.
- `Observation.ConfiguredCoverage(sel, from, to)` reports, per
  stream+source, how much of the window a collector with the selector's
  params actually ran — the difference between "nothing happened" and
  "nobody was watching".
- `Observation.Capture(sel, from, to)` pins a recorded range against
  recorder retention until closed; rule triggers create captures
  automatically (see [rules](rules.md)).
- `Observation.Retention()` reports what the recorder currently holds
  per (source, stream) — ring events/bytes, spilled segments, time span,
  detected gaps (`envspec.StreamStats`).

The recorder redacts before fan-out and storage: byte payloads that contain
a known secret are flagged and their data withheld from segment files, and
structured payloads are scrubbed through the vault redactor. Segment files
also carry loss accounting (sequence gaps, drops, restarts) as first-class
metadata so a reader can always tell evidence from absence.

## In tests

Consumers' tests use `envtest.FakeObservation`: `Publish(envspec.SignalEvent)`
feeds events (decoded from golden captures, if desired) and the fake answers
the same `Observation` interface with real selector matching. There is no
collector-authoring API.
