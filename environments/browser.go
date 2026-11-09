package environments

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

//go:embed assets/browser/Dockerfile
var browserBuild embed.FS

// BrowserBackend selects one of two pinned browser controllers.
type BrowserBackend string

const (
	PlaywrightMCP        BrowserBackend = "playwright-mcp"
	AgentBrowser         BrowserBackend = "agent-browser"
	browserProfile                      = "browser.controlled"
	maxBrowserOutput                    = 64 << 10
	maxBrowserScreenshot                = 4 << 20
)

// BrowserProfiles registers the isolated browser image with the Manager.
// The build installs pinned controller versions and their Chromium dependencies.
func BrowserProfiles() []envspec.Profile {
	return []envspec.Profile{{
		Name: browserProfile, Kind: envspec.Container,
		Description: "isolated leased Chromium with Playwright MCP and agent-browser",
		Build:       &envspec.Build{Context: browserBuild, Dockerfile: "assets/browser/Dockerfile"},
		AllowImages: []string{"node:24.11.0-bookworm@sha256:e5bbac0e9b8a6e3b96a86a82bbbcf4c533a879694fd613ed616bae5116f6f243"},
		Network:     envspec.NetworkOpen, AllowNetworks: []envspec.Network{envspec.NetworkOpen},
		Resources:    envspec.Resources{CPUs: 2, MemoryMB: 2048, DiskMB: 4096},
		MaxResources: envspec.Resources{CPUs: 4, MemoryMB: 4096, DiskMB: 8192},
	}}
}

// BrowserRequest defines one private, persistent browser. Every AcquireBrowser
// call mints a distinct service key, even for the same owner and backend.
type BrowserRequest struct {
	Backend BrowserBackend
	Network envspec.Network
}

// BrowserResult carries bounded controller output and the exact service
// generation that produced it. Truncated output is explicitly marked.
type BrowserResult struct {
	Text        string              `json:"text"`
	Truncated   bool                `json:"truncated,omitempty"`
	Service     envspec.ServiceInfo `json:"service"`
	Environment string              `json:"environment"`
	Backend     BrowserBackend      `json:"backend"`
}

// Browser holds one service borrow and serializes all browser actions.
type Browser struct {
	provider   envspec.ServiceProvider
	lease      envspec.ServiceLease
	backend    BrowserBackend
	client     *http.Client
	mcpSession string
	mcpID      int
	mu         sync.Mutex
	closed     bool
}

// AcquireBrowser creates a private browser service using the existing service
// supervisor. Call BrowserProfiles() in Config.Profiles before opening Manager.
func AcquireBrowser(ctx context.Context, provider envspec.ServiceProvider, owner envspec.Owner, req BrowserRequest) (*Browser, error) {
	if provider == nil {
		return nil, fmt.Errorf("browser provider is required")
	}
	if req.Backend != PlaywrightMCP && req.Backend != AgentBrowser {
		return nil, fmt.Errorf("unsupported browser backend %q", req.Backend)
	}
	network := req.Network
	if network == "" {
		network = envspec.NetworkOpen
	}
	if network != envspec.NetworkOpen {
		return nil, fmt.Errorf("browser profile requires open network")
	}
	spec := envspec.ServiceSpec{Name: string(req.Backend), Version: map[BrowserBackend]string{PlaywrightMCP: "0.0.84", AgentBrowser: "0.41.2"}[req.Backend], StartupTimeout: 12 * time.Minute, ProbeInterval: time.Second, ProbeTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	env := envspec.Spec{Profile: browserProfile, Network: network}
	if req.Backend == PlaywrightMCP {
		env.PublishedTCPPort = 8931
		spec.Start = []string{"env", "PLAYWRIGHT_MCP_PING_TIMEOUT_MS=0", "playwright-mcp", "--port", "8931", "--host", "0.0.0.0", "--headless", "--no-sandbox", "--executable-path", "/root/.cache/ms-playwright/chromium-1252/chrome-linux64/chrome", "--image-responses", "omit", "--output-max-size", "65536", "--output-dir", "/work/browser", "--user-data-dir", "/work/browser/profile"}
		spec.Ready = []string{"node", "-e", "fetch('http://127.0.0.1:8931/mcp').then(()=>process.exit(0)).catch(()=>process.exit(1))"}
	} else {
		spec.Start = []string{"tail", "-f", "/dev/null"}
		spec.Ready = []string{"agent-browser", "--version"}
	}
	lease, err := provider.AcquireService(ctx, owner, envspec.ServiceRequest{Key: envspec.NewID("browser_"), Environment: env, Service: spec})
	if err != nil {
		return nil, err
	}
	b := &Browser{provider: provider, lease: lease, backend: req.Backend, client: &http.Client{Timeout: 90 * time.Second}}
	if req.Backend == PlaywrightMCP {
		if err := b.initialize(ctx); err != nil {
			_ = b.Close(context.Background())
			return nil, err
		}
	}
	return b, nil
}

func (b *Browser) Info() envspec.ServiceInfo { return b.lease.ServiceInfo() }

func (b *Browser) Navigate(ctx context.Context, target string) (BrowserResult, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return BrowserResult{}, fmt.Errorf("absolute http(s) URL required")
	}
	return b.action(ctx, "browser_navigate", map[string]any{"url": target}, "open", target)
}

func (b *Browser) Snapshot(ctx context.Context) (BrowserResult, error) {
	return b.action(ctx, "browser_snapshot", map[string]any{}, "snapshot")
}

func (b *Browser) Click(ctx context.Context, target string) (BrowserResult, error) {
	if target == "" {
		return BrowserResult{}, fmt.Errorf("target is required")
	}
	return b.action(ctx, "browser_click", map[string]any{"target": target}, "click", target)
}

func (b *Browser) Fill(ctx context.Context, target, value string) (BrowserResult, error) {
	if target == "" {
		return BrowserResult{}, fmt.Errorf("target is required")
	}
	return b.action(ctx, "browser_type", map[string]any{"target": target, "text": value}, "fill", target, value)
}

func (b *Browser) Type(ctx context.Context, target, text string) (BrowserResult, error) {
	if target == "" {
		return BrowserResult{}, fmt.Errorf("target is required")
	}
	return b.action(ctx, "browser_type", map[string]any{"target": target, "text": text, "slowly": true}, "type", target, text)
}

func (b *Browser) action(ctx context.Context, tool string, args map[string]any, command ...string) (BrowserResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return BrowserResult{}, errors.New("browser is closed")
	}
	unlock, err := b.lease.Lock(ctx)
	if err != nil {
		return BrowserResult{}, err
	}
	defer unlock()
	var output string
	if b.backend == PlaywrightMCP {
		output, err = b.mcpCall(ctx, tool, args)
	} else {
		output, err = b.cliCall(ctx, command...)
	}
	if err != nil {
		return BrowserResult{}, err
	}
	truncated := len(output) > maxBrowserOutput
	if truncated {
		output = output[:maxBrowserOutput]
	}
	return BrowserResult{Text: output, Truncated: truncated, Service: b.lease.ServiceInfo(), Environment: b.lease.ID(), Backend: b.backend}, nil
}

func (b *Browser) cliCall(ctx context.Context, command ...string) (string, error) {
	argv := append([]string{"agent-browser", "--session", "leased"}, command...)
	out, stderr, exit, err := b.lease.Exec(ctx, envspec.Cmd{Argv: argv, Env: map[string]string{"AGENT_BROWSER_MAX_OUTPUT": "65536"}, Timeout: 90 * time.Second})
	if err != nil {
		return "", err
	}
	if exit != 0 {
		return "", fmt.Errorf("agent-browser exit %d: %s", exit, bounded(string(stderr), 2048))
	}
	return string(out), nil
}

func bounded(s string, limit int) string {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

func (b *Browser) mcpURL() (string, error) {
	addr := b.lease.Info().Endpoints["tcp"]
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		return "", fmt.Errorf("browser endpoint is not loopback")
	}
	return "http://" + addr + "/mcp", nil
}

func (b *Browser) initialize(ctx context.Context) error {
	_, err := b.mcpRequest(ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "environment-services", "version": "1"}}, true)
	if err != nil {
		return err
	}
	_, err = b.mcpRequest(ctx, "notifications/initialized", map[string]any{}, false)
	return err
}

func (b *Browser) mcpCall(ctx context.Context, tool string, args map[string]any) (string, error) {
	return b.mcpRequest(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, true)
}

func (b *Browser) mcpRequest(ctx context.Context, method string, params any, id bool) (string, error) {
	endpoint, err := b.mcpURL()
	if err != nil {
		return "", err
	}
	payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id {
		b.mcpID++
		payload["id"] = b.mcpID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	// Docker publishes an ephemeral host port, while the MCP server checks
	// the container-side Host value. Keep its default host restriction.
	req.Host = "127.0.0.1:8931"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if b.mcpSession != "" {
		req.Header.Set("Mcp-Session-Id", b.mcpSession)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		b.mcpSession = session
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBrowserOutput*4+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBrowserOutput*4 {
		return "", fmt.Errorf("MCP response exceeds limit")
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, bounded(string(data), 2048))
	}
	if !id {
		return "", nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "data:") {
				data = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				break
			}
		}
	}
	var reply struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return "", fmt.Errorf("invalid MCP response: %w", err)
	}
	if reply.Error != nil {
		return "", errors.New(reply.Error.Message)
	}
	var parts []string
	for _, part := range reply.Result.Content {
		if part.Type == "text" {
			parts = append(parts, part.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if reply.Result.IsError {
		return "", fmt.Errorf("browser tool failed: %s", bounded(out, 2048))
	}
	return out, nil
}

// Screenshot saves the captured PNG in the service environment and returns
// its path. Pull it through the lease before Close if raw bytes are needed.
func (b *Browser) Screenshot(ctx context.Context) (BrowserResult, error) {
	if b.backend == PlaywrightMCP {
		return b.action(ctx, "browser_take_screenshot", map[string]any{"type": "png", "filename": "browser/screenshot.png"}, "screenshot")
	}
	return b.action(ctx, "browser_take_screenshot", nil, "screenshot", "/work/browser/screenshot.png")
}

// PullScreenshot copies the last screenshot through the lease transport.
func (b *Browser) PullScreenshot(ctx context.Context, sink envspec.Sink) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("browser is closed")
	}
	if sink == nil {
		return fmt.Errorf("screenshot sink is required")
	}
	return b.lease.Pull(ctx, "/work/browser/screenshot.png", limitedBrowserSink{sink})
}

type limitedBrowserSink struct{ envspec.Sink }

func (s limitedBrowserSink) Add(path string, mode int64, reader io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(reader, maxBrowserScreenshot+1))
	if err != nil {
		return err
	}
	if len(data) > maxBrowserScreenshot {
		return fmt.Errorf("browser screenshot exceeds %d bytes", maxBrowserScreenshot)
	}
	return s.Sink.Add(path, mode, bytes.NewReader(data))
}

// Close releases the borrow and generation-fences service shutdown, which
// removes the private browser container and profile.
func (b *Browser) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	info := b.lease.ServiceInfo()
	err := b.lease.Release(ctx)
	if stopErr := b.provider.StopService(ctx, info.ID, info.Generation); err == nil {
		err = stopErr
	}
	return err
}
