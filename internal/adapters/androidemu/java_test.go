package androidemu

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func fakeJava(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "java"
	if runtime.GOOS == "windows" {
		name = "java.exe"
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", name), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The SDK's Java tools are run with JAVA_HOME when it holds a Java, then
// with the java on the PATH, then with Android Studio's bundled runtime;
// with none of them there is no Java at all.
func TestTheSDKToolsFindAJava(t *testing.T) {
	studio, set := fakeJava(t), fakeJava(t)
	onPath := false
	real := lookPath
	t.Cleanup(func() { lookPath = real })
	lookPath = func(string) (string, error) {
		if onPath {
			return "java", nil
		}
		return "", errors.New("not found")
	}

	t.Setenv("JAVA_HOME", "")
	if home, found := javaHome([]string{t.TempDir(), studio}); !found || home != studio {
		t.Fatalf("only Studio's: got %q %v, want %q", home, found, studio)
	}
	if _, found := javaHome([]string{t.TempDir()}); found {
		t.Fatal("found a Java with none anywhere")
	}
	onPath = true
	if home, found := javaHome([]string{studio}); !found || home != "" {
		t.Fatalf("java on the PATH: got %q %v, want none to set", home, found)
	}
	t.Setenv("JAVA_HOME", set)
	if home, _ := javaHome([]string{studio}); home != set {
		t.Fatalf("JAVA_HOME set: got %q, want %q", home, set)
	}
	// What the tools are run with carries it.
	env := sdkEnv("sdk", "avd")
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.EqualFold(kv, "JAVA_HOME="+set) }) {
		t.Fatalf("sdkEnv has no JAVA_HOME=%s", set)
	}
}
