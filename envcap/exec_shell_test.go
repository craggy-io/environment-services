package envcap

import (
	"runtime"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

func TestShellArgvMatchesEnvironment(t *testing.T) {
	host := shellArgv(envspec.Host, "echo hello")
	if runtime.GOOS == "windows" {
		if len(host) != 5 || host[0] != "cmd.exe" || host[3] != "/c" {
			t.Fatalf("Windows host shell = %v", host)
		}
	} else if len(host) != 3 || host[0] != "sh" || host[1] != "-c" {
		t.Fatalf("Unix host shell = %v", host)
	}
	for _, kind := range []envspec.Kind{envspec.Container, envspec.Android} {
		argv := shellArgv(kind, "echo hello")
		if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
			t.Fatalf("%s shell = %v", kind, argv)
		}
	}
}
