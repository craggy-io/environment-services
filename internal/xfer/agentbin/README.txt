Embedded craggy-xfer helper binaries. Rebuild from the module root:
  $env:GOOS='linux'; $env:GOARCH='amd64'; go build -o internal/xfer/agentbin/craggy-xfer-linux-amd64 ./cmd/xfer-agent
  $env:GOOS='linux'; $env:GOARCH='arm64'; go build -o internal/xfer/agentbin/craggy-xfer-linux-arm64 ./cmd/xfer-agent
