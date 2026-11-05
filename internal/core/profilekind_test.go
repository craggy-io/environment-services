package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/foundation-kit/bundle"
)

const userManifest = `apiVersion: craggy.io/bundle/v1
metadata:
  id: craggy.environments.user
  version: 0.1.0
contributions:
  - id: android
    kind: environments.profile@1
    path: android.json
  - id: busybox
    kind: environments.profile@1
    path: busybox.json
`

// writeBundle writes files under dir/profiles and returns dir.
func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, "profiles", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func byName(ps []envspec.Profile) map[string]envspec.Profile {
	out := map[string]envspec.Profile{}
	for _, p := range ps {
		out[p.Name] = p
	}
	return out
}

func requireCode(t *testing.T, err error, code bundle.Code) {
	t.Helper()
	var le *bundle.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a *bundle.LoadError", err)
	}
	for _, d := range le.Diagnostics {
		if d.Code == code {
			return
		}
	}
	t.Fatalf("diagnostics %+v, want code %s", le.Diagnostics, code)
}

func TestLoadProfilesNoUserFolder(t *testing.T) {
	ps, err := loadProfiles(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != len(StockProfiles()) {
		t.Fatalf("got %d profiles, want the %d stock ones", len(ps), len(StockProfiles()))
	}
}

func TestLoadProfilesUserBundleOverridesStock(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml":  userManifest,
		"android.json": `{"name":"android","kind":"android","description":"mine","android":{"api":35}}`,
		"busybox.json": `{"name":"busybox","kind":"container","image":"busybox:latest","allow_images":["busybox:*"]}`,
	})
	ps, err := loadProfiles(context.Background(), dir, []envspec.Profile{{Name: "extra", Kind: envspec.Container}})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(ps)
	if got["android"].Description != "mine" {
		t.Errorf("android = %+v, want the user profile", got["android"])
	}
	for _, name := range []string{"busybox", "extra", "linux-tools", "host-tools"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing profile %s", name)
		}
	}
	if len(ps) != len(StockProfiles())+2 {
		t.Errorf("got %d profiles, want stock + busybox + extra", len(ps))
	}
}

func TestLoadProfilesEmptyUserFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	ps, err := loadProfiles(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("empty profiles folder: %v", err)
	}
	if len(ps) != len(StockProfiles()) {
		t.Fatalf("got %d profiles, want the %d stock ones", len(ps), len(StockProfiles()))
	}
}

func TestLoadProfilesFolderWithoutManifest(t *testing.T) {
	dir := writeBundle(t, map[string]string{"android.json": `{"name":"android","kind":"android"}`})
	_, err := loadProfiles(context.Background(), dir, nil)
	requireCode(t, err, bundle.CodeManifestMissing)
	if !strings.Contains(err.Error(), "bundle.yaml") {
		t.Errorf("error %q should say a bundle.yaml is needed", err)
	}
}

func TestLoadProfilesWrongBundleID(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml":  strings.Replace(userManifest, "craggy.environments.user", "someone.else", 1),
		"android.json": `{"name":"android","kind":"android"}`,
		"busybox.json": `{"name":"busybox","kind":"container","image":"busybox:latest","allow_images":["busybox:*"]}`,
	})
	_, err := loadProfiles(context.Background(), dir, nil)
	requireCode(t, err, bundle.CodeOverrideInvalid)
}

func TestLoadProfilesNameMustMatchID(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml":  userManifest,
		"android.json": `{"name":"android","kind":"android"}`,
		"busybox.json": `{"name":"tiny","kind":"container","image":"busybox:latest","allow_images":["busybox:*"]}`,
	})
	_, err := loadProfiles(context.Background(), dir, nil)
	requireCode(t, err, bundle.CodeCompileFailed)
	if !strings.Contains(err.Error(), "core: load profiles") {
		t.Errorf("error %q lacks context", err)
	}
}

func TestLoadProfilesRejectsList(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"bundle.yaml":  userManifest,
		"android.json": `[{"name":"android","kind":"android"}]`,
		"busybox.json": `{"name":"busybox","kind":"container","image":"busybox:latest","allow_images":["busybox:*"]}`,
	})
	_, err := loadProfiles(context.Background(), dir, nil)
	requireCode(t, err, bundle.CodeCompileFailed)
}

func TestLoadProfilesConfigClashIsAnError(t *testing.T) {
	_, err := loadProfiles(context.Background(), t.TempDir(), []envspec.Profile{{Name: "android", Kind: envspec.Android}})
	requireCode(t, err, bundle.CodeIDConflict)
}
