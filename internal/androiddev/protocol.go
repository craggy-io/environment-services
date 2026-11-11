package androiddev

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

type packageProtocolBackend interface {
	Snapshot(context.Context, string) (envspec.PackageSnapshot, error)
	Install(context.Context, io.Reader, bool) error
	Uninstall(context.Context, string) error
}

type devicePackageBackend struct{ d *Device }

func (b devicePackageBackend) Snapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error) {
	return b.d.PackageSnapshot(ctx, pkg)
}
func (b devicePackageBackend) Install(ctx context.Context, apk io.Reader, replace bool) error {
	return b.d.Install(ctx, apk, replace)
}
func (b devicePackageBackend) Uninstall(ctx context.Context, pkg string) error {
	return b.d.Uninstall(ctx, pkg)
}

// ApplyPackageSequence performs an exact install/uninstall order and returns a
// host receipt for every transition. It stops at the first failed or
// identity-mismatched step; a partial receipt is returned with the error.
func (d *Device) ApplyPackageSequence(ctx context.Context, steps []envspec.PackageMutation) (envspec.PackageSequenceReceipt, error) {
	return applyPackageSequence(ctx, devicePackageBackend{d}, steps)
}

func applyPackageSequence(ctx context.Context, backend packageProtocolBackend, steps []envspec.PackageMutation) (envspec.PackageSequenceReceipt, error) {
	started := time.Now().UTC()
	receipt := envspec.PackageSequenceReceipt{StartedAt: started.Format(time.RFC3339Nano)}
	if len(steps) == 0 {
		return receipt, fmt.Errorf("android: package sequence is empty")
	}
	for i, step := range steps {
		if err := validatePackageName(step.Package); err != nil {
			return receipt, fmt.Errorf("android: package step %d: %w", i, err)
		}
		switch step.Operation {
		case "install":
			if len(step.APK) == 0 {
				return receipt, fmt.Errorf("android: package step %d: install APK is empty", i)
			}
		case "uninstall":
			if len(step.APK) != 0 {
				return receipt, fmt.Errorf("android: package step %d: uninstall must not carry an APK", i)
			}
		default:
			return receipt, fmt.Errorf("android: package step %d: operation must be install or uninstall", i)
		}

		before, err := backend.Snapshot(ctx, step.Package)
		if err != nil {
			return receipt, fmt.Errorf("android: package step %d before: %w", i, err)
		}
		stepStarted := time.Now().UTC()
		switch step.Operation {
		case "install":
			err = backend.Install(ctx, bytes.NewReader(step.APK), step.Replace)
		case "uninstall":
			err = backend.Uninstall(ctx, step.Package)
		}
		if err != nil {
			return receipt, fmt.Errorf("android: package step %d %s %s: %w", i, step.Operation, step.Package, err)
		}
		after, err := backend.Snapshot(ctx, step.Package)
		if err != nil {
			return receipt, fmt.Errorf("android: package step %d after: %w", i, err)
		}
		switch step.Operation {
		case "install":
			if !after.Installed || len(after.CodeSHA256) != 64 {
				return receipt, fmt.Errorf("android: package step %d did not establish installed code identity", i)
			}
		case "uninstall":
			if after.Installed {
				return receipt, fmt.Errorf("android: package step %d left %s installed", i, step.Package)
			}
		}
		completed := time.Now().UTC()
		receipt.Steps = append(receipt.Steps, envspec.PackageMutationReceipt{Index: i, Operation: step.Operation,
			Package: step.Package, StartedAt: stepStarted.Format(time.RFC3339Nano),
			CompletedAt: completed.Format(time.RFC3339Nano), Before: before, After: after})
	}
	receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return receipt, nil
}

var packageNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

func validatePackageName(pkg string) error {
	if !packageNamePattern.MatchString(pkg) {
		return fmt.Errorf("invalid Android package name %q", pkg)
	}
	return nil
}

// PackageSnapshot reads installed code identity and uid for pkg. Not installed
// is a successful snapshot with Installed false.
func (d *Device) PackageSnapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error) {
	if err := validatePackageName(pkg); err != nil {
		return envspec.PackageSnapshot{}, err
	}
	out, code, err := d.Shell(ctx, "pm path "+pkg)
	if err != nil {
		return envspec.PackageSnapshot{}, err
	}
	result := envspec.PackageSnapshot{Package: pkg}
	if code != 0 || strings.TrimSpace(out) == "" {
		return result, nil
	}
	for _, line := range strings.Split(out, "\n") {
		path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if strings.HasSuffix(path, "/base.apk") || result.CodePath == "" {
			result.CodePath = path
		}
		if strings.HasSuffix(path, "/base.apk") {
			break
		}
	}
	if !strings.HasPrefix(result.CodePath, "/") {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: pm path %s returned no absolute code path", pkg)
	}
	sum, code, err := d.RootShell(ctx, "sha256sum "+adbQuote(result.CodePath))
	if err != nil || code != 0 {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: hash installed %s: exit %d: %s: %w", pkg, code, strings.TrimSpace(sum), err)
	}
	fields := strings.Fields(sum)
	if len(fields) == 0 || len(fields[0]) != 64 {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: invalid installed digest for %s", pkg)
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: invalid installed digest for %s: %w", pkg, err)
	}
	result.CodeSHA256 = strings.ToLower(fields[0])
	result.UID, err = packageUID(ctx, d, pkg)
	if err != nil {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: installed package %s uid: %w", pkg, err)
	}
	if result.UID <= 0 {
		return envspec.PackageSnapshot{}, fmt.Errorf("android: installed package %s has no observed uid", pkg)
	}
	result.Installed = true
	return result, nil
}

// DecodeFullBackup decodes the streamed tarball retained below LocalTransport's
// _full directory. Android's transport API defines this object as a streamed
// tarball; malformed or truncated objects are rejected rather than searched as
// unstructured bytes.
func DecodeFullBackup(data []byte) ([]envspec.BackupEntry, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	var entries []envspec.BackupEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("android: decode full-backup tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		payload, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("android: read full-backup entry %s: %w", h.Name, err)
		}
		sum := sha256.Sum256(payload)
		entries = append(entries, envspec.BackupEntry{Name: h.Name, Size: h.Size,
			SHA256: hex.EncodeToString(sum[:]), Bytes: payload})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("android: full-backup tar contained no regular files")
	}
	return entries, nil
}

type backupProtocolBackend interface {
	Shell(context.Context, string) (string, int, error)
	RootPaths(context.Context, string) ([]string, error)
	ReadRootFile(context.Context, string) ([]byte, error)
}

type deviceBackupBackend struct{ d *Device }

func (b deviceBackupBackend) Shell(ctx context.Context, cmd string) (string, int, error) {
	return b.d.Shell(ctx, cmd)
}
func (b deviceBackupBackend) RootPaths(ctx context.Context, pkg string) ([]string, error) {
	roots := []string{
		"/data/user_de/0/com.android.localtransport/files",
		"/data/user/0/com.android.localtransport/files",
	}
	// LocalTransport does not create both credential- and device-encrypted
	// roots on every Android build. Guard each root independently so a normal
	// missing directory does not turn an otherwise informative empty result
	// into an opaque `find ... exit 1` protocol failure.
	cmd := "for root in " + strings.Join(roots, " ") + "; do " +
		"if [ -d \"$root\" ]; then find \"$root\" -type f \\( " +
		"-path '*/1/_delta/" + pkg + "/*' -o -path '*/1/_full/" + pkg + "' \\) -print; fi; done"
	out, code, err := b.d.RootShell(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if code != 0 && strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("find local-transport objects: exit %d", code)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if path := strings.TrimSpace(line); strings.HasPrefix(path, "/data/") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}
func (b deviceBackupBackend) ReadRootFile(ctx context.Context, path string) ([]byte, error) {
	return b.d.readRootFile(ctx, path)
}

// BackupToLocalTransport selects Android's debug-only local transport,
// performs one immediate package backup, reads back the exact recipient-owned
// objects, and restores the previously selected transport.
func (d *Device) BackupToLocalTransport(ctx context.Context, pkg string) (envspec.BackupReceipt, error) {
	return backupToLocalTransport(ctx, deviceBackupBackend{d}, pkg)
}

func backupToLocalTransport(ctx context.Context, backend backupProtocolBackend, pkg string) (receipt envspec.BackupReceipt, result error) {
	if err := validatePackageName(pkg); err != nil {
		return receipt, err
	}
	receipt.Package = pkg
	receipt.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	enabledOut, code, err := backend.Shell(ctx, "bmgr enabled")
	if err != nil || code != 0 {
		return receipt, fmt.Errorf("android: read backup-manager state (exit %d): %s: %w", code, strings.TrimSpace(enabledOut), err)
	}
	receipt.BackupEnabled = backupManagerEnabled(enabledOut)
	if !receipt.BackupEnabled {
		out, enableCode, enableErr := backend.Shell(ctx, "bmgr enable true")
		if enableErr != nil || enableCode != 0 {
			return receipt, fmt.Errorf("android: enable backup manager (exit %d): %s: %w", enableCode, strings.TrimSpace(out), enableErr)
		}
		receipt.EnabledByHost = true
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			out, disableCode, disableErr := backend.Shell(cleanup, "bmgr enable false")
			if disableErr != nil || disableCode != 0 {
				result = errors.Join(result, fmt.Errorf("android: restore disabled backup manager (exit %d): %s: %w", disableCode, strings.TrimSpace(out), disableErr))
			}
		}()
	}
	list, code, err := backend.Shell(ctx, "bmgr list transports")
	if err != nil || code != 0 {
		return receipt, fmt.Errorf("android: list backup transports (exit %d): %s: %w", code, strings.TrimSpace(list), err)
	}
	previous, local := parseBackupTransports(list)
	if local == "" {
		return receipt, fmt.Errorf("android: debug-only LocalTransport is unavailable")
	}
	receipt.Transport, receipt.PreviousTransport = local, previous
	receipt.Recipient = "android-local-transport:" + local
	selected, code, err := backend.Shell(ctx, "bmgr transport "+local)
	if err != nil || code != 0 {
		return receipt, fmt.Errorf("android: select backup transport (exit %d): %s: %w", code, strings.TrimSpace(selected), err)
	}
	if previous != "" && previous != local {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			out, restoreCode, restoreErr := backend.Shell(cleanup, "bmgr transport "+previous)
			if restoreErr != nil || restoreCode != 0 {
				result = errors.Join(result, fmt.Errorf("android: restore backup transport (exit %d): %s: %w", restoreCode, strings.TrimSpace(out), restoreErr))
			}
		}()
	}
	wipe, code, err := backend.Shell(ctx, "bmgr wipe "+local+" "+pkg)
	if err != nil || code != 0 {
		return receipt, fmt.Errorf("android: wipe prior backup (exit %d): %s: %w", code, strings.TrimSpace(wipe), err)
	}
	out, code, err := backend.Shell(ctx, "bmgr backupnow --monitor "+pkg)
	if err == nil && backupRejectedAtPreflight(out) {
		// backupnow can report `Success` for the aggregate operation (for
		// example because the @pm@ metadata package completed) while the
		// requested application was rejected at preflight. Package-level
		// rejection is decisive and must be checked before the aggregate
		// success marker.
		return receipt, fmt.Errorf("android: %w (exit %d): %s", envspec.ErrBackupRejected, code, strings.TrimSpace(out))
	}
	if err != nil || code != 0 || !backupSucceeded(out) {
		return receipt, fmt.Errorf("android: package backup failed (exit %d): %s: %w", code, strings.TrimSpace(out), err)
	}
	receipt.CommandOutput = strings.TrimSpace(out)
	paths, err := backend.RootPaths(ctx, pkg)
	if err != nil {
		return receipt, err
	}
	if len(paths) == 0 {
		return receipt, fmt.Errorf("android: %w for %s after bmgr reported success: %s",
			envspec.ErrBackupRecipientEmpty, pkg, receipt.CommandOutput)
	}
	for _, path := range paths {
		data, err := backend.ReadRootFile(ctx, path)
		if err != nil {
			return receipt, fmt.Errorf("android: read backup object %s: %w", path, err)
		}
		sum := sha256.Sum256(data)
		object := envspec.BackupObject{Path: path, SHA256: hex.EncodeToString(sum[:]), Bytes: data}
		if strings.Contains(path, "/_full/") {
			object.Entries, err = DecodeFullBackup(data)
			if err != nil {
				return receipt, fmt.Errorf("android: decode backup object %s: %w", path, err)
			}
		}
		receipt.Objects = append(receipt.Objects, object)
	}
	receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return receipt, nil
}

func parseBackupTransports(out string) (active, local string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		selected := strings.HasPrefix(line, "*")
		name := strings.TrimSpace(strings.TrimPrefix(line, "*"))
		if selected {
			active = name
		}
		lower := strings.ToLower(name)
		if local == "" && (strings.Contains(lower, "com.android.localtransport") || strings.Contains(name, "LocalTransport")) {
			local = name
		}
	}
	return active, local
}

func backupSucceeded(out string) bool {
	lower := strings.ToLower(out)
	return !strings.Contains(lower, "fail") && (strings.Contains(lower, "success") || strings.Contains(lower, "result: 0"))
}

// backupRejectedAtPreflight recognises the backup-manager monitor output
// that reports a preflight/transport rejection of the package.
func backupRejectedAtPreflight(out string) bool {
	up := strings.ToUpper(out)
	return strings.Contains(up, "ERROR_PREFLIGHT") ||
		strings.Contains(up, "TRANSPORT REJECTED PACKAGE") ||
		strings.Contains(up, "BACKUP_MANAGER_POLICY")
}

func backupManagerEnabled(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "currently enabled") || strings.TrimSpace(lower) == "enabled"
}

func (d *Device) readRootFile(ctx context.Context, path string) ([]byte, error) {
	if !strings.HasPrefix(path, "/data/") || strings.ContainsAny(path, "\r\n\x00") {
		return nil, fmt.Errorf("android: invalid root file path %q", path)
	}
	rc, closeFn, err := d.tr.ExecOut(ctx, d.cfg.Serial, d.RootCmd("cat -- "+adbQuote(path)), nil)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	return io.ReadAll(rc)
}
