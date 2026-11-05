package androiddev

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

type actionProtocolFake struct {
	snapshot envspec.PackageSnapshot
	nonce    string
}

func (f *actionProtocolFake) Snapshot(context.Context, string) (envspec.PackageSnapshot, error) {
	return f.snapshot, nil
}
func (f *actionProtocolFake) Shell(_ context.Context, cmd string) (string, int, error) {
	marker := "--es cmd '"
	start := strings.Index(cmd, marker)
	if start < 0 {
		return "Error: missing command", 0, nil
	}
	encoded := cmd[start+len(marker):]
	encoded = strings.TrimSuffix(encoded, "'")
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	var envelope packageActionEnvelope
	_ = json.Unmarshal(raw, &envelope)
	f.nonce = envelope.Nonce
	return "Status: ok", 0, nil
}
func (f *actionProtocolFake) RootShell(_ context.Context, cmd string) (string, int, error) {
	if f.nonce != "" && strings.Contains(cmd, f.nonce) {
		return `{"allowed":true}`, 0, nil
	}
	return "", 1, nil
}

func TestInvokePackageActionReceiptsStableIdentityAndResult(t *testing.T) {
	fake := &actionProtocolFake{snapshot: envspec.PackageSnapshot{Package: "lab.example.holder", Installed: true, UID: 10123, CodeSHA256: strings.Repeat("a", 64)}}
	receipt, err := invokePackageAction(context.Background(), fake, envspec.PackageAction{Package: "lab.example.holder", Activity: ".ActionActivity", Command: json.RawMessage(`{"uri":"content://target/value"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Before.UID != 10123 || receipt.After.UID != 10123 || len(receipt.ResultSHA256) != 64 || string(receipt.Result) != `{"allowed":true}` {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestInvokePackageActionRejectsShellFragments(t *testing.T) {
	fake := &actionProtocolFake{}
	_, err := invokePackageAction(context.Background(), fake, envspec.PackageAction{Package: "lab.example.holder;id", Activity: ".Act", Command: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected invalid package error")
	}
}
