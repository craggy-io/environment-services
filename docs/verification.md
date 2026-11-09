# Verification and limits

[Project](../README.md) · [Documentation index](README.md) · [Architecture diagram](assets/environment-services-architecture.svg)

The module requires Go 1.27 or newer. These checks exercise the public API,
package boundaries and simulated runtime failures:

```sh
go build ./...
go vet ./...
go test ./... -count=1
```

On 2026-11-09, all three commands passed on Windows when run as an isolated
module with `GOWORK=off`. The full test suite includes the
`TestNoArtifactServicesModule` import-graph guard. From the parent Craggy
workspace without `GOWORK=off`, that one test fails because the parent
`go.work` places sibling modules, including artifact-services, in the module
graph. This is a workspace-scoping failure; it does not establish that an
environment-services package imports artifact-services. The package layering
checks and isolated module suite passed.

The normal suite covers host lifecycle, handle fencing, durable operations,
service supervision, fake Android and Docker adapter behavior, signal
collection and rules. It does not exercise a physical phone or a live Docker
daemon by default. Successful local tests do not certify a deployment,
hardware model, operating system or recovery after power loss.

## Live Docker and browsers

Live Docker adapter tests are opt-in:

```powershell
$env:CRAGGY_TEST_DOCKER = '1'
$env:GOWORK = 'off'
go test ./internal/adapters/docker -count=1 -v
```

They require a responding Docker daemon and may pull or build images. The
80,000-file transfer gate is skipped with `-short`; the backend-start test
also needs `CRAGGY_TEST_DOCKER_BOOT=1` and a machine where Docker Desktop is
stopped. These tests are separate from the normal suite.

The [leased browser](browsers.md) test is also opt-in. It builds the pinned
browser profile on first use, unless `ENVIRONMENT_BROWSER_TEST_IMAGE` names
an image already built from the supplied Dockerfile:

```powershell
$env:ENVIRONMENT_BROWSER_LIVE = '1'
$env:GOWORK = 'off'
go test ./environments -run '^TestBrowserLive$' -count=1 -v -timeout 20m
```

On 2026-11-09, both Playwright MCP and agent-browser passed the controlled
local-page flow using a prebuilt image from that Dockerfile: navigation,
accessibility snapshots, fill, type, click, screenshot transfer, cookie
persistence in one lease, cookie isolation between two leases of the same
owner, and generation-fenced service shutdown. The Dockerfile itself built
successfully. The first-build path through `AcquireBrowser` was not fully
timed end to end; a cold image build exceeded the original three-minute
startup setting, which is now twelve minutes. The browser API is currently a
Go library API; Craggy Labs agent capability wiring and target-specific
authorization are separate work.

## Android hardware

Android emulator and physical-device adapter tests use fake transports for
many lifecycle and collector cases. A physical device requires an Android
SDK path, ADB authorization, an explicit serial and an exclusive attach
lease. The implementation disconnects physical devices on release and
does not destroy or reset them; see [environments](environments.md) and
[capabilities](capabilities.md).

The adapter has separate opt-in live tests:

```powershell
$env:CRAGGY_TEST_ANDROID_EMU = '1'
go test ./internal/adapters/androidemu -count=1 -v

$env:CRAGGY_TEST_ANDROID_PHYS_SERIAL = '<authorised adb serial>'
go test ./internal/adapters/androidphys -count=1 -v

$env:CRAGGY_TEST_ANDROID_XFER = '1'
$env:CRAGGY_ADB_SERIAL = '<adb serial>'   # emulator or physical device
go test ./internal/androiddev -run TestLiveArchive -count=1 -v
```

The emulator lifecycle test needs an installed Android SDK and prepared
system image, and creates its own emulator; the physical and transfer tests
use the named, already-connected device.

On 2026-11-09 the archive-transfer tests (`TestLiveArchive*`: a tree with
binary, empty, nested, space-named and 4 MiB files pushed and pulled through
the device's `tar` over adb, single-file pull, overwrite, workspace-relative
paths and a failing source) passed on a Pixel 6 and on a running
`sdk_gphone64_x86_64` emulator. The physical tests (`TestLivePhysicalAttach`
and `TestLivePhysicalTransferThroughCore`, which attaches through the core
manager and moves a tree with `Handle.Push`/`Pull`) passed on the same
Pixel 6. The emulator lifecycle test was not run.

The absence of a tested backend is an unverified deployment path, not a
reason to infer success from simulated tests. Check each target environment
under its intended operator and policy before depending on it.
