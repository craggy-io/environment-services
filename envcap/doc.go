// Package envcap is the capability surface over environment
// services: Capabilities returns registry.Capability values ready for
// Registry.Add, so one registration projects the surface to every
// registry projection (HTTP, MCP, tools, Starlark). It is the only
// package in this module that imports capability-services.
//
// envcap sits INSIDE the trust boundary. Its Deps.Manager interface
// deliberately includes capability-internal operations that the
// consumer facade (environments.Manager) does not expose — borrows,
// by-ID exec/transfer, credential administration, secret typing —
// because the host assembling envcap has already decided its agents
// may do these things. Never wire Deps from untrusted input, and map
// callers to owners only through Deps.Owner (trusted invocation
// context), never from capability arguments.
//
// envcap does not import environments: Deps.Manager is satisfied
// structurally, in practice by environments.NewEnvCapDeps. It also
// imports no Starlark: consumers assemble the Starlark surface
// themselves (capability-services' starlarkcap). Artifact storage is
// injected through the Artifacts port; this module has no
// artifact-services dependency.
package envcap
