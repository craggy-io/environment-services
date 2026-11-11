package androiddev

import (
	"context"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

func TestPackageUIDExactNameAndAndroid25Fallback(t *testing.T) {
	const pkg = "lab.craggy.target"
	const dump = "Packages:\n  Package [lab.craggy.target.extra] (a):\n    userId=99999\n  Package [lab.craggy.target] (b):\n    userId=10123\n    pkg=Package{b lab.craggy.target}\n  Package [lab.craggy.target.other] (c):\n    userId=88888\n"
	for _, tc := range []struct {
		name, pmOutput string
		pmCode         int
		want           int
	}{
		{"current pm", "package:lab.craggy.target.extra uid:99999\npackage:lab.craggy.target uid:10123\n", 0, 10123},
		{"Android 25", "Error: Unknown option: -U\n", 1, 10123},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTransport{answer: func(cmd string) (string, int, error) {
				switch cmd {
				case "pm list packages -U " + pkg:
					return tc.pmOutput, tc.pmCode, nil
				case "dumpsys package " + pkg:
					return dump, 0, nil
				default:
					t.Fatalf("unexpected command %q", cmd)
					return "", 1, nil
				}
			}}
			uid, err := packageUID(context.Background(), New(f, Config{Serial: "emulator-5554"}), pkg)
			if err != nil || uid != tc.want {
				t.Fatalf("uid = %d, %v; want %d", uid, err, tc.want)
			}
			if tc.pmCode == 0 && len(f.commands) != 1 {
				t.Fatalf("current pm unexpectedly used fallback: %v", f.commands)
			}
		})
	}
}

func TestPackageUIDFallbackRejectsUnrelatedBlockAndOtherFailures(t *testing.T) {
	const pkg = "lab.craggy.target"
	for _, tc := range []struct {
		name, pmOutput, dump string
		pmCode               int
		want                 string
	}{
		{"missing exact block", "Error: Unknown option: -U", "Package [lab.craggy.target.other] (a):\n  userId=10123\n", 1, "exact package uid not found"},
		{"other pm failure", "Permission denied", "", 1, "exit 1: Permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTransport{answer: func(cmd string) (string, int, error) {
				if strings.HasPrefix(cmd, "pm list packages -U ") {
					return tc.pmOutput, tc.pmCode, nil
				}
				if strings.HasPrefix(cmd, "dumpsys package ") {
					return tc.dump, 0, nil
				}
				t.Fatalf("unexpected command %q", cmd)
				return "", 1, nil
			}}
			_, err := packageUID(context.Background(), New(f, Config{Serial: "emulator-5554"}), pkg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.name == "other pm failure" && len(f.commands) != 1 {
				t.Fatalf("unrelated failure triggered fallback: %v", f.commands)
			}
		})
	}
}

func TestPackageSnapshotUsesAndroid25UIDFallback(t *testing.T) {
	const pkg = "lab.craggy.target"
	f := &fakeTransport{answer: func(cmd string) (string, int, error) {
		switch {
		case cmd == "pm path "+pkg:
			return "package:/data/app/lab.craggy.target-1/base.apk\n", 0, nil
		case strings.HasPrefix(cmd, "sha256sum "):
			return strings.Repeat("a", 64) + "  /data/app/lab.craggy.target-1/base.apk\n", 0, nil
		case cmd == "pm list packages -U "+pkg:
			return "Error: Unknown option: -U", 1, nil
		case cmd == "dumpsys package "+pkg:
			return "Packages:\n  Package [lab.craggy.target] (b):\n    userId=10123\n", 0, nil
		default:
			t.Fatalf("unexpected command %q", cmd)
			return "", 1, nil
		}
	}}
	snapshot, err := New(f, Config{Serial: "emulator-5554", Root: envspec.RootADB}).PackageSnapshot(context.Background(), pkg)
	if err != nil || !snapshot.Installed || snapshot.UID != 10123 || snapshot.CodeSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
}
