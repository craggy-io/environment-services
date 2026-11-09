# Leased browsers

`environments.BrowserProfiles()` registers one Docker build containing pinned
`@playwright/mcp@0.0.84`, `playwright@1.65.0-alpha-2026-10-30`, and
`agent-browser@0.41.2` on a digest-pinned `node:24.11.0-bookworm`. Register it when opening
the manager:

```go
m, err := environments.Open(ctx, environments.Config{
    Dir: stateDir, Profiles: environments.BrowserProfiles(),
})
if err != nil { return err }
browser, err := environments.AcquireBrowser(ctx, m, owner, environments.BrowserRequest{
    Backend: environments.PlaywrightMCP, // or environments.AgentBrowser
})
if err != nil { return err }
defer browser.Close(context.Background())
_, err = browser.Navigate(ctx, "https://example.com")
if err != nil { return err }
snapshot, err := browser.Snapshot(ctx)
```

Each acquisition mints a new service key, so even the same owner receives a
separate container, browser profile, cookies, tabs and service generation.
Actions on one `Browser` are serialized through its service lease. `Close`
releases the borrow and stops that exact generation. The service supervisor
also reaps an idle service after its timeout. State persists across actions
while the lease is held, but closing the browser destroys the container. The
API does not restore cookies into a later lease.

Both backends expose navigate, accessibility snapshot, click, fill, type and
screenshot. The Playwright backend calls the pinned MCP server through its
loopback-published HTTP endpoint. The agent-browser backend executes its
version-pinned CLI inside the lease; the CLI keeps a daemon and Chrome session
between commands. `BrowserResult` includes backend, service generation and
environment ID; text output is capped at 64 KiB and marks truncation.
Screenshots are saved at `/work/browser/screenshot.png` and transferred by
`PullScreenshot` before closing. The caller owns any retention or artifact
record created from that transfer.

This is a library API, not a registry capability. Consumers decide which
agents may acquire a browser, which URLs are in scope, and whether any
result or screenshot should be kept. The supplied profile uses open network
access for browser navigation. The MCP port is published only on host
loopback; the browser environment itself has egress.

To run the controlled-page live test with Docker, set
`ENVIRONMENT_BROWSER_LIVE=1` and run
`go test ./environments -run '^TestBrowserLive$' -count=1 -v -timeout 20m`.
For a faster rerun against an image built from the supplied Dockerfile, set
`ENVIRONMENT_BROWSER_TEST_IMAGE` to that image reference. The live test
verifies form interaction, a screenshot transfer, cookie persistence within
one lease, isolation across two leases owned by the same agent, and cleanup.
