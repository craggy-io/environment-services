package redact_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/internal/redact"
)

func TestSecretNeverPrints(t *testing.T) {
	s := redact.NewSecret("hunter2-password")
	forms := []string{s.String(), s.GoString(), fmt.Sprintf("%v", s), fmt.Sprintf("%s", s), fmt.Sprintf("%+v", s)}
	b, _ := json.Marshal(map[string]redact.Secret{"password": s})
	forms = append(forms, string(b))
	for _, f := range forms {
		if strings.Contains(f, "hunter2") {
			t.Fatalf("secret leaked in %q", f)
		}
	}
	if !strings.Contains(string(b), "[redacted]") {
		t.Fatalf("json = %s", b)
	}
}

func TestRedactorFormsAndSignalShapes(t *testing.T) {
	r := redact.New()
	r.Hold("env:x", "cred_1", map[string]redact.Secret{"password": redact.NewSecret("Correct Horse 42!")})
	for _, in := range []string{
		"pw is Correct Horse 42! ok",
		"b64 " + base64.StdEncoding.EncodeToString([]byte("Correct Horse 42!")),
	} {
		out := r.RedactString(in)
		if strings.Contains(out, "Correct Horse") || !strings.Contains(out, "[secret:cred_1/password]") {
			t.Fatalf("redact(%q) = %q", in, out)
		}
	}
	if got := r.RedactString("short tp"); got != "short tp" {
		t.Fatalf("text with no secret changed: %q", got)
	}
	if !r.ContainsSecret([]byte("xx Correct Horse 42! yy")) {
		t.Fatal("ContainsSecret missed pcap-style bytes")
	}
	// Signal.Redactor shape: maps scrubbed, bytes passed through.
	m := r.Redact("s@1", map[string]any{"text": "Correct Horse 42!"})
	if strings.Contains(fmt.Sprintf("%v", m), "Correct Horse") {
		t.Fatalf("map redact failed: %v", m)
	}
	raw := []byte("binary Correct Horse 42! bytes")
	got, _ := r.Redact("net.pcap", raw).([]byte)
	if string(got) != string(raw) {
		t.Fatal("byte payloads must pass through untouched")
	}
}

func TestRedactorForgetsPerHolder(t *testing.T) {
	r := redact.New()
	secret := map[string]redact.Secret{"password": redact.NewSecret("Correct Horse 42!")}
	r.Hold("env:x", "cred_1", secret)
	r.Hold("env:y", "cred_1", secret)
	r.Hold("env:y", "cred_2", map[string]redact.Secret{"api_key": redact.NewSecret("sk-0123456789")})
	raw := []byte("Correct Horse 42!")
	r.Forget("env:x")
	if !r.ContainsSecret(raw) {
		t.Fatal("a secret another holder still has must stay matched")
	}
	r.Forget("env:y")
	if r.ContainsSecret(raw) || r.ContainsSecret([]byte("sk-0123456789")) {
		t.Fatal("the redactor must forget what no holder has")
	}
	r.Forget("env:never")
}
