package xfer

import (
	"bytes"
	"embed"
)

//go:embed agentbin
var agentBin embed.FS

// AgentBinary returns the embedded craggy-xfer helper for a GOARCH
// ("amd64", "arm64"), or false when the build did not embed one (callers
// then fall back to the full-tar path of §5.2). Build with:
//
//	GOOS=linux GOARCH=amd64 go build -o internal/xfer/agentbin/craggy-xfer-linux-amd64 ./cmd/xfer-agent
//	GOOS=linux GOARCH=arm64 go build -o internal/xfer/agentbin/craggy-xfer-linux-arm64 ./cmd/xfer-agent
func AgentBinary(goarch string) ([]byte, bool) {
	b, err := agentBin.ReadFile("agentbin/craggy-xfer-linux-" + goarch)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// AgentDigest digests the embedded helper so it can be cached in an
// environment under its own digest (§5.3).
func AgentDigest(goarch string) (string, bool) {
	b, ok := AgentBinary(goarch)
	if !ok {
		return "", false
	}
	d, _, _ := DigestOf(bytes.NewReader(b))
	return d, true
}
