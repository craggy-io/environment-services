package androidemu

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An image install that says nothing went wrong but leaves no image is an
// error, as the sdkmanager wrapper of the Android CLI does with a
// semicolon path; and the CLI is used when the command-line tools have it.
func TestAnImageInstallThatLeavesNoImageFails(t *testing.T) {
	sdk := t.TempDir()
	bin := filepath.Join(sdk, "cmdline-tools", "latest", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	name, body := "sdkmanager", "#!/bin/sh\necho Package system-images not found.\nexit 0\n"
	if runtime.GOOS == "windows" {
		name, body = "sdkmanager.bat", "@echo Package system-images not found.\r\n@exit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Adapter{SDK: sdk}
	if cli := d.androidCLI(); cli != "" {
		t.Fatalf("found a CLI that isn't there: %s", cli)
	}
	err := d.installImage(context.Background(), "system-images;android-34;google_apis;x86_64")
	if err == nil || !strings.Contains(err.Error(), "left no image") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v, want an error saying no image was left, with what the tool said", err)
	}

	cli := "android"
	if runtime.GOOS == "windows" {
		cli += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, cli), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := d.androidCLI(); got != filepath.Join(bin, cli) {
		t.Fatalf("CLI: got %q", got)
	}
}
