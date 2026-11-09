package environments

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

func TestBrowserRequestValidation(t *testing.T) {
	if _, err := AcquireBrowser(context.Background(), nil, envspec.Owner{}, BrowserRequest{}); err == nil {
		t.Fatal("nil provider accepted")
	}
	if len(BrowserProfiles()) != 1 || BrowserProfiles()[0].Build == nil {
		t.Fatal("browser build profile missing")
	}
	m, err := Open(context.Background(), Config{Dir: t.TempDir(), Profiles: BrowserProfiles()})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background(), ShutdownDrain); err != nil {
		t.Fatal(err)
	}
}

// Run with ENVIRONMENT_BROWSER_LIVE=1 and Docker available. The target is a
// controlled page on the host; each backend receives its own service lease.
func TestBrowserLive(t *testing.T) {
	if os.Getenv("ENVIRONMENT_BROWSER_LIVE") != "1" {
		t.Skip("set ENVIRONMENT_BROWSER_LIVE=1")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mark", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "browser_lease", Value: "private-cookie", Path: "/"})
		fmt.Fprint(w, "marked")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		marker := "unmarked"
		if cookie, err := r.Cookie("browser_lease"); err == nil {
			marker = cookie.Value
		}
		fmt.Fprintf(w, `<html><body><label>Name <input aria-label="Name"></label><button onclick="document.querySelector('#result').textContent=document.querySelector('input').value">Submit</button><p id="result">Waiting</p><p>%s</p></body></html>`, marker)
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	profiles := BrowserProfiles()
	if image := os.Getenv("ENVIRONMENT_BROWSER_TEST_IMAGE"); image != "" {
		profiles[0].Build = nil
		profiles[0].Image = image
		profiles[0].AllowImages = []string{image}
	}
	m, err := Open(ctx, Config{Dir: t.TempDir(), Profiles: profiles})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background(), ShutdownDrain)
	url := fmt.Sprintf("http://host.docker.internal:%d/", listener.Addr().(*net.TCPAddr).Port)
	for _, backend := range []BrowserBackend{PlaywrightMCP, AgentBrowser} {
		t.Run(string(backend), func(t *testing.T) {
			owner := envspec.Owner{ID: "agent:browser-test", Session: string(backend)}
			browser, err := AcquireBrowser(ctx, m, owner, BrowserRequest{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close(context.Background())
			if _, err := browser.Navigate(ctx, url); err != nil {
				t.Fatal(err)
			}
			snap, err := browser.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(snap.Text, "Name") || !strings.Contains(snap.Text, "Submit") {
				t.Fatalf("snapshot = %q", snap.Text)
			}
			input := browserRef(t, snap.Text, "Name", backend)
			button := browserRef(t, snap.Text, "Submit", backend)
			if _, err := browser.Fill(ctx, input, "lease works"); err != nil {
				t.Fatal(err)
			}
			if _, err := browser.Type(ctx, input, "!"); err != nil {
				t.Fatal(err)
			}
			if _, err := browser.Click(ctx, button); err != nil {
				t.Fatal(err)
			}
			result, err := browser.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Text, "lease works!") {
				t.Fatalf("result = %q", result.Text)
			}
			screenshot, err := browser.Screenshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("screenshot: %s", screenshot.Text)
			sink := envspec.NewMemSink()
			if err := browser.PullScreenshot(ctx, sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.Files) == 0 {
				t.Fatal("screenshot missing")
			}
			if _, err := browser.Navigate(ctx, url+"mark"); err != nil {
				t.Fatal(err)
			}
			if _, err := browser.Navigate(ctx, url); err != nil {
				t.Fatal(err)
			}
			marked, err := browser.Snapshot(ctx)
			if err != nil || !strings.Contains(marked.Text, "private-cookie") {
				t.Fatalf("cookie did not persist: %q %v", marked.Text, err)
			}
			other, err := AcquireBrowser(ctx, m, owner, BrowserRequest{Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(context.Background())
			if other.Info().ID == browser.Info().ID {
				t.Fatal("distinct leases shared a browser service")
			}
			if _, err := other.Navigate(ctx, url); err != nil {
				t.Fatal(err)
			}
			clean, err := other.Snapshot(ctx)
			if err != nil || strings.Contains(clean.Text, "private-cookie") {
				t.Fatalf("cookie crossed leases: %q %v", clean.Text, err)
			}
			if err := other.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if other.Info().State != "stopped" {
				t.Fatalf("other service state = %q", other.Info().State)
			}
			if err := browser.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if browser.Info().State != "stopped" {
				t.Fatalf("browser service state = %q", browser.Info().State)
			}
		})
	}
}

func browserRef(t *testing.T, snapshot, label string, backend BrowserBackend) string {
	t.Helper()
	for _, line := range strings.Split(snapshot, "\n") {
		if !strings.Contains(line, label) {
			continue
		}
		match := regexp.MustCompile(`\b(?:ref=|@)(e[0-9]+)\b`).FindStringSubmatch(line)
		if len(match) == 2 {
			if backend == AgentBrowser {
				return "@" + match[1]
			}
			return match[1]
		}
	}
	t.Fatalf("no ref for %q in %q", label, snapshot)
	return ""
}
