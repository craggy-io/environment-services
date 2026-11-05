package androiddev

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

const packageActionLimit = 32 << 10

type packageActionEnvelope struct {
	Nonce   string          `json:"nonce"`
	Command json.RawMessage `json:"command"`
}

type packageActionBackend interface {
	Snapshot(context.Context, string) (envspec.PackageSnapshot, error)
	Shell(context.Context, string) (string, int, error)
	RootShell(context.Context, string) (string, int, error)
}

type deviceActionBackend struct{ d *Device }

func (b deviceActionBackend) Snapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error) {
	return b.d.PackageSnapshot(ctx, pkg)
}
func (b deviceActionBackend) Shell(ctx context.Context, cmd string) (string, int, error) {
	return b.d.Shell(ctx, cmd)
}
func (b deviceActionBackend) RootShell(ctx context.Context, cmd string) (string, int, error) {
	return b.d.RootShell(ctx, cmd)
}

var relativeActivityPattern = regexp.MustCompile(`^\.[A-Za-z][A-Za-z0-9_.$]*$`)

// ApplyPackageSequenceAndAction applies every mutation, then invokes the
// holder action without an unreceipted package transition between them.
func (d *Device) ApplyPackageSequenceAndAction(ctx context.Context, steps []envspec.PackageMutation, action envspec.PackageAction) (envspec.PackageSequenceActionReceipt, error) {
	var receipt envspec.PackageSequenceActionReceipt
	sequence, err := d.ApplyPackageSequence(ctx, steps)
	receipt.Sequence = sequence
	if err != nil {
		return receipt, err
	}
	receipt.Action, err = invokePackageAction(ctx, deviceActionBackend{d}, action)
	return receipt, err
}

func invokePackageAction(ctx context.Context, backend packageActionBackend, action envspec.PackageAction) (envspec.PackageActionReceipt, error) {
	var receipt envspec.PackageActionReceipt
	if err := validatePackageName(action.Package); err != nil {
		return receipt, err
	}
	if !relativeActivityPattern.MatchString(action.Activity) {
		return receipt, fmt.Errorf("android: invalid package-relative activity %q", action.Activity)
	}
	if len(action.Command) == 0 || !json.Valid(action.Command) {
		return receipt, fmt.Errorf("android: package action command must be valid JSON")
	}
	before, err := backend.Snapshot(ctx, action.Package)
	if err != nil {
		return receipt, fmt.Errorf("android: package action identity before: %w", err)
	}
	if !before.Installed {
		return receipt, fmt.Errorf("android: package action package %s is not installed", action.Package)
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return receipt, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	raw, err := json.Marshal(packageActionEnvelope{Nonce: nonce, Command: action.Command})
	if err != nil {
		return receipt, err
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if len(encoded) > packageActionLimit {
		return receipt, fmt.Errorf("android: package action command is too large (%d bytes encoded, limit %d)", len(encoded), packageActionLimit)
	}
	started := time.Now().UTC()
	receipt = envspec.PackageActionReceipt{Package: action.Package, Activity: action.Activity,
		StartedAt: started.Format(time.RFC3339Nano), Before: before}
	out, code, err := backend.Shell(ctx, fmt.Sprintf("am start -W -f 0x18000000 -n %s/%s --es cmd '%s'", action.Package, action.Activity, encoded))
	if err != nil {
		return receipt, fmt.Errorf("android: start package action (exit %d): %s: %w", code, strings.TrimSpace(out), err)
	}
	if code != 0 || strings.Contains(out, "Error:") {
		return receipt, fmt.Errorf("android: start package action (exit %d): %s", code, strings.TrimSpace(out))
	}
	timeout := action.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	path := "/data/data/" + action.Package + "/files/craggy-actions/" + nonce + ".json"
	for {
		result, _, readErr := backend.RootShell(ctx, "cat "+path+" 2>/dev/null && rm -f "+path)
		if readErr != nil {
			return receipt, fmt.Errorf("android: read package action result: %w", readErr)
		}
		result = strings.TrimSpace(result)
		if result != "" {
			if len(result) > packageActionLimit || !json.Valid([]byte(result)) {
				return receipt, fmt.Errorf("android: package action returned invalid or oversized JSON")
			}
			receipt.Result = append(json.RawMessage(nil), result...)
			break
		}
		if time.Now().After(deadline) {
			return receipt, fmt.Errorf("android: package action wrote no result within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	after, err := backend.Snapshot(ctx, action.Package)
	if err != nil {
		return receipt, fmt.Errorf("android: package action identity after: %w", err)
	}
	if !after.Installed || after.UID != before.UID || after.CodeSHA256 != before.CodeSHA256 {
		return receipt, fmt.Errorf("android: package action identity changed during execution")
	}
	receipt.After = after
	sum := sha256.Sum256(receipt.Result)
	receipt.ResultSHA256 = hex.EncodeToString(sum[:])
	receipt.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return receipt, nil
}
