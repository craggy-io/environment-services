package androiddev

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

func fullBackupTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for name, value := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(value))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type packageProtocolFake struct {
	state    map[string]envspec.PackageSnapshot
	installs []string
	step     int
	actions  []string
}

func (f *packageProtocolFake) Snapshot(_ context.Context, pkg string) (envspec.PackageSnapshot, error) {
	if s, ok := f.state[pkg]; ok {
		return s, nil
	}
	return envspec.PackageSnapshot{Package: pkg}, nil
}

func (f *packageProtocolFake) Install(_ context.Context, apk io.Reader, _ bool) error {
	if f.step >= len(f.installs) {
		return errors.New("unexpected install")
	}
	b, _ := io.ReadAll(apk)
	if len(b) == 0 {
		return errors.New("empty apk")
	}
	pkg := f.installs[f.step]
	f.step++
	f.actions = append(f.actions, "install:"+pkg)
	f.state[pkg] = envspec.PackageSnapshot{Package: pkg, Installed: true, UID: 10000 + f.step,
		CodePath: "/data/app/" + pkg + "/base.apk", CodeSHA256: strings.Repeat("a", 64)}
	return nil
}

func (f *packageProtocolFake) Uninstall(_ context.Context, pkg string) error {
	f.actions = append(f.actions, "uninstall:"+pkg)
	f.state[pkg] = envspec.PackageSnapshot{Package: pkg}
	return nil
}

func TestApplyPackageSequenceReceiptsExactOrder(t *testing.T) {
	f := &packageProtocolFake{state: map[string]envspec.PackageSnapshot{}, installs: []string{"lab.attacker", "lab.target"}}
	steps := []envspec.PackageMutation{
		{Operation: "install", Package: "lab.attacker", APK: []byte("attacker")},
		{Operation: "install", Package: "lab.target", APK: []byte("target")},
		{Operation: "uninstall", Package: "lab.attacker"},
	}
	receipt, err := applyPackageSequence(context.Background(), f, steps)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(f.actions, ","), "install:lab.attacker,install:lab.target,uninstall:lab.attacker"; got != want {
		t.Fatalf("actions = %s, want %s", got, want)
	}
	if len(receipt.Steps) != 3 || receipt.Steps[0].After.UID == 0 || receipt.Steps[2].After.Installed {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.StartedAt == "" || receipt.CompletedAt == "" {
		t.Fatalf("receipt times missing: %+v", receipt)
	}
}

type packageBackendFunc struct {
	snapshot  func(context.Context, string) (envspec.PackageSnapshot, error)
	install   func(context.Context, io.Reader, bool) error
	uninstall func(context.Context, string) error
}

func (f packageBackendFunc) Snapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error) {
	return f.snapshot(ctx, pkg)
}
func (f packageBackendFunc) Install(ctx context.Context, apk io.Reader, replace bool) error {
	return f.install(ctx, apk, replace)
}
func (f packageBackendFunc) Uninstall(ctx context.Context, pkg string) error {
	return f.uninstall(ctx, pkg)
}

func TestApplyPackageSequenceRejectsUnreceiptedIdentity(t *testing.T) {
	fake := packageBackendFunc{
		snapshot: func(_ context.Context, pkg string) (envspec.PackageSnapshot, error) {
			return envspec.PackageSnapshot{Package: pkg, Installed: true}, nil
		},
		install:   func(context.Context, io.Reader, bool) error { return nil },
		uninstall: func(context.Context, string) error { return nil },
	}
	_, err := applyPackageSequence(context.Background(), fake, []envspec.PackageMutation{{Operation: "install", Package: "lab.target", APK: []byte("apk")}})
	if err == nil || !strings.Contains(err.Error(), "installed code identity") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidatePackageName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{{"com.example.app", true}, {"a.b", true}, {"com.example;id", false}, {"single", false}, {"../x", false}} {
		if err := validatePackageName(tc.name); (err == nil) != tc.ok {
			t.Fatalf("validatePackageName(%q) = %v", tc.name, err)
		}
	}
}

type backupProtocolFake struct {
	commands    []string
	paths       []string
	files       map[string][]byte
	failAt      string
	backupNtOut string // overrides the `bmgr backupnow` output when set
}

func (f *backupProtocolFake) Shell(_ context.Context, cmd string) (string, int, error) {
	f.commands = append(f.commands, cmd)
	if cmd == f.failAt {
		return "broken", 1, nil
	}
	switch cmd {
	case "bmgr enabled":
		return "Backup Manager currently disabled", 0, nil
	case "bmgr enable true", "bmgr enable false":
		return "Backup Manager now enabled", 0, nil
	case "bmgr list transports":
		return "* cloud/Transport\n  com.android.localtransport/.LocalTransport", 0, nil
	case "bmgr transport com.android.localtransport/.LocalTransport", "bmgr transport cloud/Transport":
		return "Selected transport", 0, nil
	case "bmgr wipe com.android.localtransport/.LocalTransport com.example.app":
		return "Package wiped", 0, nil
	case "bmgr backupnow --monitor com.example.app":
		if f.backupNtOut != "" {
			return f.backupNtOut, 0, nil
		}
		return "Backup finished with result: Success", 0, nil
	default:
		return "", 1, errors.New("unexpected command: " + cmd)
	}
}

func (f *backupProtocolFake) RootPaths(context.Context, string) ([]string, error) {
	return append([]string(nil), f.paths...), nil
}

func (f *backupProtocolFake) ReadRootFile(_ context.Context, path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, errors.New("missing file")
	}
	return append([]byte(nil), data...), nil
}

func TestBackupToLocalTransportRetainsRecipientBytesAndRestoresTransport(t *testing.T) {
	path := "/data/user_de/0/com.android.localtransport/files/1/_delta/com.example.app/c2VjcmV0"
	f := &backupProtocolFake{paths: []string{path}, files: map[string][]byte{path: []byte("private-canary")}}
	receipt, err := backupToLocalTransport(context.Background(), f, "com.example.app")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Recipient != "android-local-transport:com.android.localtransport/.LocalTransport" || len(receipt.Objects) != 1 ||
		!bytes.Equal(receipt.Objects[0].Bytes, []byte("private-canary")) || len(receipt.Objects[0].SHA256) != 64 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := f.commands[len(f.commands)-2:]; !slices.Equal(got, []string{"bmgr transport cloud/Transport", "bmgr enable false"}) {
		t.Fatalf("cleanup commands = %q", got)
	}
}

func TestBackupToLocalTransportDecodesFullBackupEntries(t *testing.T) {
	path := "/data/user_de/0/com.android.localtransport/files/1/_full/com.example.app"
	tarball := fullBackupTar(t, map[string]string{"apps/com.example.app/f/files/craggy-canary": "private-canary"})
	f := &backupProtocolFake{paths: []string{path}, files: map[string][]byte{path: tarball}}
	receipt, err := backupToLocalTransport(context.Background(), f, "com.example.app")
	if err != nil {
		t.Fatal(err)
	}
	entries := receipt.Objects[0].Entries
	if len(entries) != 1 || entries[0].Name != "apps/com.example.app/f/files/craggy-canary" ||
		!bytes.Equal(entries[0].Bytes, []byte("private-canary")) || len(entries[0].SHA256) != 64 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestDecodeFullBackupRejectsMalformedStream(t *testing.T) {
	if _, err := DecodeFullBackup([]byte("not a tar stream")); err == nil {
		t.Fatal("malformed full-backup stream was accepted")
	}
}

func TestBackupToLocalTransportReportsPreflightRejection(t *testing.T) {
	f := &backupProtocolFake{backupNtOut: "Package @pm@ with result: Success\nPackage com.example.app with result: BackupManagerMonitor{...BACKUP_MANAGER_POLICY / ERROR_PREFLIGHT...}\nTransport rejected package because it wasn't able to process it at the time\nBackup finished with result: Success"}
	_, err := backupToLocalTransport(context.Background(), f, "com.example.app")
	if err == nil || !errors.Is(err, envspec.ErrBackupRejected) {
		t.Fatalf("error = %v, want wraps ErrBackupRejected", err)
	}
	// The transport is still restored even though the package was rejected.
	if got := f.commands[len(f.commands)-2:]; !slices.Equal(got, []string{"bmgr transport cloud/Transport", "bmgr enable false"}) {
		t.Fatalf("cleanup commands = %q", got)
	}
}

func TestBackupToLocalTransportReportsEmptyRecipientAfterSuccess(t *testing.T) {
	f := &backupProtocolFake{}
	receipt, err := backupToLocalTransport(context.Background(), f, "com.example.app")
	if err == nil || !errors.Is(err, envspec.ErrBackupRecipientEmpty) {
		t.Fatalf("error = %v, want wraps ErrBackupRecipientEmpty", err)
	}
	if receipt.CommandOutput != "Backup finished with result: Success" {
		t.Fatalf("command output = %q", receipt.CommandOutput)
	}
	if !strings.Contains(err.Error(), receipt.CommandOutput) {
		t.Fatalf("error %q does not retain bmgr output", err)
	}
}

func TestBackupToLocalTransportRestoresTransportAfterFailure(t *testing.T) {
	f := &backupProtocolFake{failAt: "bmgr backupnow --monitor com.example.app"}
	_, err := backupToLocalTransport(context.Background(), f, "com.example.app")
	if err == nil || !strings.Contains(err.Error(), "package backup failed") {
		t.Fatalf("error = %v", err)
	}
	if got := f.commands[len(f.commands)-2:]; !slices.Equal(got, []string{"bmgr transport cloud/Transport", "bmgr enable false"}) {
		t.Fatalf("cleanup commands = %q", got)
	}
}

func TestParseBackupTransports(t *testing.T) {
	active, local := parseBackupTransports("  one/Cloud\n* android/com.android.internal.backup.LocalTransport\n")
	if active != "android/com.android.internal.backup.LocalTransport" || local != active {
		t.Fatalf("active/local = %q/%q", active, local)
	}
	if backupSucceeded("Backup finished: FAILURE") || !backupSucceeded("Backup finished with result: Success") {
		t.Fatal("backup success classification is not strict")
	}
	if backupManagerEnabled("Backup Manager currently disabled") || !backupManagerEnabled("Backup Manager currently enabled") {
		t.Fatal("backup-manager state classification is not strict")
	}
}
