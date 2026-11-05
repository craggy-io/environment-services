package androiddev

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

func TestParseIPCLine(t *testing.T) {
	cases := []struct {
		line string
		want envspec.IPCEvent
		ok   bool
	}{
		{"I/ActivityTaskManager: START u0 {act=android.intent.action.MAIN cmp=com.example/.Main (has extras)} with LAUNCH_MULTIPLE from uid 10193 result code=0",
			envspec.IPCEvent{Kind: envspec.IPCActivity, CallerUID: 10193, Action: "android.intent.action.MAIN", Component: "com.example/.Main"}, true},
		{"I ActivityManager: START u0 {act=android.intent.action.VIEW dat=content://victim/doc flg=0x1 cmp=com.victim/.Export} from uid 10200",
			envspec.IPCEvent{Kind: envspec.IPCActivity, CallerUID: 10200, Action: "android.intent.action.VIEW", Data: "content://victim/doc", Flags: "0x1", GrantRead: true, Component: "com.victim/.Export"}, true},
		{"I/ActivityManager: some unrelated line", envspec.IPCEvent{}, false},
	}
	for _, tc := range cases {
		ev, ok := parseIPCLine(tc.line)
		if ok != tc.ok {
			t.Fatalf("parse %q ok=%v want %v", tc.line, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if ev.Kind != tc.want.Kind || ev.CallerUID != tc.want.CallerUID || ev.Action != tc.want.Action ||
			ev.Component != tc.want.Component || ev.Data != tc.want.Data || ev.GrantRead != tc.want.GrantRead {
			t.Fatalf("parse %q = %+v, want %+v", tc.line, ev, tc.want)
		}
		if ev.Raw == "" {
			t.Fatal("raw line not retained")
		}
	}
}

// logcatFrame builds one binary logcat frame.
func logcatFrame(pid, tid int, prio byte, tag, msg string) []byte {
	payload := append([]byte{prio}, tag...)
	payload = append(payload, 0)
	payload = append(payload, msg...)
	payload = append(payload, 0)
	hdr := make([]byte, 28)
	binary.LittleEndian.PutUint16(hdr[0:2], uint16(len(payload)))
	binary.LittleEndian.PutUint16(hdr[2:4], 28)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(pid))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(tid))
	binary.LittleEndian.PutUint32(hdr[12:16], 1700000000)
	return append(hdr, payload...)
}

func TestLogcatCollectorEmitsAndFailsOnStreamEnd(t *testing.T) {
	frames := append(logcatFrame(42, 43, 6, "Taggy", "hello crash"), logcatFrame(7, 7, 4, "Sys", "booted")...)
	f := &fakeTransport{
		devices: []adb.Device{{Serial: "emulator-5554", State: "device"}},
		answer: func(cmd string) (string, int, error) {
			if strings.Contains(cmd, "date +%s") {
				return "1700000000", 0, nil
			}
			return "", 0, nil
		},
		execOut: func(cmd string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(string(frames))), nil
		},
	}
	dev := New(f, Config{Serial: "emulator-5554", Root: envspec.RootADB})
	var got []envspec.SignalEvent
	col := &logcatCollector{dev: dev}
	err := col.Run(context.Background(), nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) { got = append(got, ev) }))
	if err == nil {
		t.Fatal("stream end must be the collector failure (core restarts it)")
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	line, _ := got[0].Data.(envspec.LogLine)
	if line.Text != "hello crash" || line.Tag != "Taggy" || line.Priority != "ERROR" || line.PID != 42 {
		t.Fatalf("line = %+v", line)
	}
}

// crashFake scripts the shell answers the crash collector needs.
func crashFake(files map[string]string, statLine string) *fakeTransport {
	return &fakeTransport{
		devices: []adb.Device{{Serial: "emulator-5554", State: "device"}},
		answer: func(cmd string) (string, int, error) {
			switch {
			case strings.Contains(cmd, "boot_id"):
				return "boot-1234\n", 0, nil
			case strings.Contains(cmd, "stat -c"):
				return statLine, 0, nil
			case strings.HasPrefix(strings.TrimSpace(cmd), "su -c") && strings.Contains(cmd, "cat"):
				// root cat handled below via generic cat
				fallthrough
			default:
				for path, content := range files {
					if strings.Contains(cmd, "cat") && strings.Contains(cmd, path) {
						return content, 0, nil
					}
				}
				if strings.Contains(cmd, "sha256sum") {
					return strings.Repeat("b", 64) + "  /x\n", 0, nil
				}
				if strings.Contains(cmd, "pm path") {
					return "package:/data/app/com.crashy/base.apk\n", 0, nil
				}
				return "", 0, nil
			}
		},
	}
}

func TestCrashCollectorBaselineThenNewTombstone(t *testing.T) {
	tomb := "Cmdline: com.crashy\npid: 1234, tid: 1234, name: main  >>> com.crashy <<<\nsignal 11 (SIGSEGV)\n"
	var stat atomic.Value
	stat.Store("")
	f := crashFake(map[string]string{"/data/tombstones/tombstone_01": tomb}, "")
	f.blockOn = "logcat -b crash"
	f.answer = func(cmd string) (string, int, error) {
		switch {
		case strings.Contains(cmd, "boot_id"):
			return "boot-1\n", 0, nil
		case strings.Contains(cmd, "/data/tombstones") && strings.Contains(cmd, "stat"):
			return stat.Load().(string), 0, nil
		case strings.Contains(cmd, "cat") && strings.Contains(cmd, "tombstone_01"):
			return tomb, 0, nil
		case strings.Contains(cmd, "sha256sum"):
			return strings.Repeat("b", 64) + " x\n", 0, nil
		case strings.Contains(cmd, "pm path"):
			return "package:/data/app/com.crashy/base.apk\n", 0, nil
		default:
			return "", 0, nil
		}
	}
	dev := New(f, Config{Serial: "emulator-5554", Root: envspec.RootADB})
	col := &crashCollector{dev: dev, Interval: 20 * time.Millisecond}
	var mu sync.Mutex
	var events []envspec.CrashEvent
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- col.Run(ctx, nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) {
			if ce, ok := ev.Data.(envspec.CrashEvent); ok {
				mu.Lock()
				events = append(events, ce)
				mu.Unlock()
			}
		}))
	}()
	time.Sleep(60 * time.Millisecond) // baseline cycle(s): no files
	stat.Store("128 1700000000 /data/tombstones/tombstone_01\n")
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, e := range events {
			if e.Kind == "tombstone" && e.Tombstone != nil && len(e.Tombstone.Bytes) > 0 {
				found = true
			}
		}
		mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no tombstone event; events=%+v cmds=%v", events, f.commands)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	var ts *envspec.CrashEvent
	for i := range events {
		if events[i].Kind == "tombstone" && events[i].Tombstone != nil && len(events[i].Tombstone.Bytes) > 0 {
			ts = &events[i]
		}
	}
	if ts == nil {
		t.Fatal("tombstone event missing")
	}
	sum := sha256.Sum256([]byte(tomb))
	if ts.Tombstone.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %s, want %s", ts.Tombstone.SHA256, hex.EncodeToString(sum[:]))
	}
	if ts.Process != "com.crashy" || ts.PID != 1234 || ts.Signal != "SIGSEGV" || ts.BootID != "boot-1" {
		t.Fatalf("event = %+v", ts)
	}
	if ts.Tombstone.Package != "com.crashy" || ts.Tombstone.PackageCodeSHA256 != strings.Repeat("b", 64) {
		t.Fatalf("package identity = %+v", ts.Tombstone)
	}
}

func TestParseStateAndMemoryParams(t *testing.T) {
	if _, err := parseStateParams(json.RawMessage(`{"packages":["com.example.app"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := parseStateParams(json.RawMessage(`{"packages":["bad;rm"]}`)); err == nil {
		t.Fatal("shell-fragment package accepted")
	}
	if _, err := parseStateParams(json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty packages accepted")
	}
	p, err := parseMemoryParams(json.RawMessage(`{"package":"com.example.app"}`))
	if err != nil || p.MaxMappingBytes == 0 || p.MaxTotalBytes < p.MaxMappingBytes {
		t.Fatalf("memory params = %+v err=%v", p, err)
	}
	if _, err := parseMemoryParams(json.RawMessage(`{"package":"com.example.app","max_mapping_bytes":10}`)); err == nil {
		t.Fatal("sub-page mapping cap accepted")
	}
	if _, err := parseFSStateParams(json.RawMessage(`{"roots":["relative/path"]}`)); err == nil {
		t.Fatal("relative root accepted")
	}
}

func TestFridaCollectorValidationAndSessionReader(t *testing.T) {
	p := fridaParams{Script: "x", Package: "com.a", Process: "p"}
	if err := p.validate(); err == nil {
		t.Fatal("package+process accepted")
	}
	p = fridaParams{Script: "x", Package: "com.a", Spawn: true}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	var got []envspec.SignalEvent
	input := `{"kind":"loaded","package":"com.a"}` + "\n" + `{"kind":"checkpoint","requestedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:01Z"}` + "\n"
	if err := readFridaSession(strings.NewReader(input), signalcore.EmitterFunc(func(ev envspec.SignalEvent) {
		got = append(got, ev)
	})); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("frida events = %d", len(got))
	}
	bad := `{"kind":"checkpoint","requestedAt":"2026-01-01T00:00:01Z","completedAt":"2026-01-01T00:00:00Z"}` + "\n"
	if err := readFridaSession(strings.NewReader(bad), signalcore.EmitterFunc(func(ev envspec.SignalEvent) {})); err == nil {
		t.Fatal("backwards checkpoint accepted")
	}
}

func TestCollectorsRegistryCoversAndroidStreams(t *testing.T) {
	dev := New(&fakeTransport{}, Config{Serial: "s", Root: envspec.RootADB})
	want := map[string]bool{
		envspec.StreamAndroidLogcat: false, envspec.StreamAndroidProc: false,
		envspec.StreamAndroidPackage: false, envspec.StreamAndroidCrash: false,
		envspec.StreamAndroidState: false, envspec.StreamAndroidMemory: false,
		envspec.StreamAndroidBinder: false, envspec.StreamAndroidIPC: false,
		envspec.StreamAndroidFrida: false, envspec.StreamFSObserve: false,
		envspec.StreamEnvResources: false, envspec.StreamEnvHealth: false,
		envspec.StreamEnvProcess: false, envspec.StreamFSActivity: false,
		envspec.StreamNetPCAP: false,
	}
	for _, c := range dev.Collectors() {
		for _, sd := range c.Streams() {
			if _, ok := want[sd.Name]; ok {
				want[sd.Name] = true
			}
			if sd.Name != envspec.StreamNetPCAP && sd.Schema == nil {
				t.Errorf("collector %s stream %s has no schema", c.Name(), sd.Name)
			}
		}
	}
	for s, ok := range want {
		if !ok {
			t.Errorf("stream %s not provided", s)
		}
	}
	names := map[string]bool{}
	for _, p := range dev.Probes() {
		names[p.Name] = true
		if p.Check == nil || p.Repair == nil || p.BeforeRepair == nil {
			t.Errorf("probe %s incomplete (check/repair/before-repair)", p.Name)
		}
	}
	for _, n := range []string{"adb", "root"} {
		if !names[n] {
			t.Errorf("probe %s missing", n)
		}
	}
}
