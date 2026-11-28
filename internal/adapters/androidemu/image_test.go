package androidemu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

// A variant's image is the one the SDK has, however its platform is named:
// android-34, or since API 37 by minor release too (android-37.0). One
// not there is installed under the name sdkmanager knows it by.
func TestAVariantUsesTheImageAsTheSDKNamesIt(t *testing.T) {
	sdk := t.TempDir()
	have := func(release, flavour string) {
		dir := filepath.Join(sdk, "system-images", release, flavour, "x86_64")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.xml"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := &Adapter{SDK: sdk}
	opts := func(api int, img envspec.ImageKind) *envspec.AndroidOptions {
		return &envspec.AndroidOptions{API: api, Image: img, ABI: "x86_64"}
	}

	have("android-37.0", "google_apis_playstore")
	have("android-34", "google_apis")
	have("android-36", "google_apis")
	have("android-36.1", "google_apis")
	for _, c := range []struct {
		o    *envspec.AndroidOptions
		want string
		in   bool
	}{
		{opts(37, envspec.GooglePlay), "system-images;android-37.0;google_apis_playstore;x86_64", true},
		{opts(34, envspec.GoogleAPIs), "system-images;android-34;google_apis;x86_64", true},
		{opts(36, envspec.GoogleAPIs), "system-images;android-36;google_apis;x86_64", true},
		{opts(37, envspec.GoogleAPIs), "system-images;android-37.0;google_apis;x86_64", false},
		{opts(35, envspec.GoogleAPIs), "system-images;android-35;google_apis;x86_64", false},
	} {
		if got := d.imagePackage(c.o); got != c.want {
			t.Errorf("API %d %s: got %s, want %s", c.o.API, c.o.Image, got, c.want)
		}
		if got := d.imageInstalled(c.o); got != c.in {
			t.Errorf("API %d %s installed: got %v, want %v", c.o.API, c.o.Image, got, c.in)
		}
	}
}
