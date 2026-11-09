# Documentation

[Project](../README.md) · [Verification](verification.md)

The guides describe the public Go contracts, lifecycle behavior, runtime
adapters, signals and agent capability projection. Start with
[environments](environments.md) when opening a manager, or
[envspec](envspec.md) when declaring a profile or depending on a narrow
interface.

## Public APIs

- [Environment manager](environments.md): `Open`, provision, leases,
  transfer, supervised services, profiles and runtime selection.
- [Specs and records](envspec.md): standard-library-only contracts shared
  with consumers.
- [Capabilities](capabilities.md): the `envcap` registry projection and its
  trust boundary.
- [Leased browsers](browsers.md): private Playwright MCP and agent-browser
  services exposed through the Go library API.

## Behavior and operations

- [Signals](signals.md): collectors, pins, coverage, query and capture.
- [Rules](rules.md): bounded Starlark signal rules and triggers.
- [Design](design.md): lifecycle and observation separation, persistence,
  adapters and cleanup.
- [Verification](verification.md): executed checks and unqualified paths.

The [design notes](design.md) describe the current cutover boundary. The
implementation guides above describe the supported API.
