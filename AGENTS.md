# Environment Services: coding-agent integration guide

Use this guide when editing this repository or integrating its Go module,
`github.com/craggy-io/environment-services`, into a consumer. Repository
development rules below apply to work here. Consumers should explicitly
provide this guide to their coding agent; installing a dependency does not
imply its AGENTS.md is read.

Design authority: [docs/plan.md](docs/plan.md) (lifecycle design), as amended
by the governing decisions D1–D9 in
[docs/execution-plan.md](docs/execution-plan.md) §0. This is a full cutover:
new `craggy.*` identities only, no legacy adoption, migration, compatibility
projections, shims or deprecation.

## Choose the owning package

| Need | Package and contract |
| --- | --- |
| Declare specs/profiles; name records (payloads, receipts, host snapshots) | `envspec` — stdlib-only |
| Provision, release, observe, run services | `environments` (`Open` + `Manager`) |
| Expose environments as registry capabilities | `envcap` (handlers over envcap-owned interfaces) |
| Fake observation in a consumer's tests | `envtest` (`FakeObservation`) — tests only |
| Durable lifecycle, leases, admission, operations | `internal/core` + `internal/store` |
| Profile loading (`environments.profile@1`, stock provider, `<Dir>/profiles` bundle) | `internal/core/profilekind.go` on foundation-kit `bundle` |
| A runtime (docker, host, android emulator/physical) | `internal/adapters/*` behind `internal/adapter` |
| Signal hub, recorder, ranges | `internal/signalcore` (each adapter owns its collectors) |
| Starlark signal rules | `internal/rules` on script-services |
| Credential leasing and redaction | `internal/vault` (`Secret.Reveal` never leaves `internal/`) |
| File-tree transfer | `internal/xfer`; in-environment agent is `cmd/xfer-agent` |

Public surface is exactly `envspec`, `environments`, `envcap` (production)
plus `envtest` (test-only). Everything else lives under `internal/`.
Package guides: [docs/envspec.md](docs/envspec.md),
[docs/environments.md](docs/environments.md), [docs/signals.md](docs/signals.md),
[docs/rules.md](docs/rules.md), [docs/capabilities.md](docs/capabilities.md),
[docs/design.md](docs/design.md). Consumer cutover steps (FREEZE, gates,
platform, clean drain) are in [docs/cutover.md](docs/cutover.md).

## Import rules

Dependencies point one way and are enforced by `arch_test.go`
(`TestEnvSpecStdlibOnly`, `TestLayering`):

- `envspec` imports **only the standard library**. This is load-bearing:
  consumers' domain packages alias its record types and must not pull
  drivers, SQLite or Docker clients transitively. Never add an import to it.
- Adapters import `internal/adapter` + `envspec` + their own technology and
  register collectors against the `internal/signalcore` collector contract.
  Collectors receive typed ports from their own adapter — never cross-adapter
  concrete types, never a stringly `Handle(key) any`.
- Adapters never import `internal/core`, `environments`, `envcap` or each
  other. `signalcore` imports only `envspec`. `core` never imports adapters,
  moby or gopacket.
- `envcap` imports only `envspec` + capability-services/registry. It never
  imports `environments` or any `internal/` package; `environments` satisfies
  envcap's `Deps` interfaces structurally.
- `capability-services` only in `envcap` (+ `examples/demo`).
  `script-services` and `go.starlark.net` only in `internal/rules`.
- Profiles load only through foundation-kit `bundle.Load` in
  `internal/core` (`loadProfiles`). Never add a second reader of profile
  files. `<Dir>/profiles` must be a bundle with a written `bundle.yaml`
  (`metadata.id: craggy.environments.user`, one JSON profile per file);
  never generate a manifest, and a folder without one is an error. See
  [docs/environments.md](docs/environments.md#profiles).
- **No artifact-services import anywhere.** The artifact Stage/Keep port is
  owned by `envcap`; consumers implement it over their artifact stack.
- `environments` is the only assembly root. `examples/demo` imports only
  `environments` + `envcap` + `envspec`.

## Identities and deletion

- Everything this code creates or recognises uses `craggy.*` identities
  (labels, names, metadata keys). Foreign resources are never adopted:
  adapters mark them `Foreign` in `Discover` results and the core skips
  them. There is no adoption or legacy-warning API; draining a previous
  stack is an operator task ([docs/cutover.md](docs/cutover.md)).
- Fully remove obsolete code and call sites rather than adding deprecation
  markers, compatibility shims, aliases or commented-out remnants. If it is
  gone, it is gone.
- Stream payload JSON is frozen at its current versions (`android.crash@2`,
  …): no field renames. `envspec` golden tests pin the wire shapes.

## Developing and verifying in this repository

- Preserve concurrent edits and reread before patching. Use `rg` for searches.
- Check existing helpers before adding boilerplate. On the second occurrence
  of a pattern, extract or widen a small shared helper at the narrowest useful
  scope; name helpers for what they guarantee.
- After changing a data model in `envspec`, trace every reader, formatter,
  JSON tag and downstream consumer; silent field omissions are bugs. When you
  move or rename a type, grep the whole tree for consumers before finishing.
- Verify fixes by running the affected code and existing tests. If coverage
  is missing, add and run a minimal regression test or reproduction. Report
  what ran and any limits; do not equate an unrun check with a pass.

From this module root, using the Go version specified in [go.mod](go.mod):

```sh
go build ./...
go vet ./...
go test ./... -count=1
```

The gate is those three commands green (the `arch_test.go` layering matrix
runs inside `go test`), `gofmt` applied to touched files, and no legacy
module-path or label strings in `*.go` (the historical design documents
`docs/plan.md`/`docs/execution-plan.md` and the old-stack drain commands in
`docs/cutover.md` still name the old stack for operators; they are not
identities this code creates or recognises). When touching lifecycle,
signal or concurrency code, also run
`go test ./internal/core/ ./internal/signalcore/ -race -count=1` and
`deadcode -test ./...` (zero findings; delete real dead code, never mark it
deprecated). Live tests (real Docker daemon / Android device) are gated
behind their build tags and environment; say so when they were skipped.
