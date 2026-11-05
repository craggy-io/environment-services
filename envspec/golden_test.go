package envspec_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

// Golden files under testdata/ were emitted by marshalling representative
// values with the predecessor implementation (a throwaway generator over
// the old module, run once at S1). Each case must decode into the new type
// with no unknown fields and re-marshal to semantically identical JSON:
// the stream payload contract is frozen (no field renames).

func goldenCases() map[string]func() any {
	return map[string]func() any{
		"spec":                            func() any { return &envspec.Spec{} },
		"profile":                         func() any { return &envspec.Profile{} },
		"service_info":                    func() any { return &envspec.ServiceInfo{} },
		"owner":                           func() any { return &envspec.Owner{} },
		"entry":                           func() any { return &envspec.Entry{} },
		"progress":                        func() any { return &envspec.Progress{} },
		"resources":                       func() any { return &envspec.ResourcesData{} },
		"log_line":                        func() any { return &envspec.LogLine{} },
		"fs_change":                       func() any { return &envspec.FSChange{} },
		"proc_event":                      func() any { return &envspec.ProcEvent{} },
		"package_event":                   func() any { return &envspec.PackageEvent{} },
		"health_event":                    func() any { return &envspec.HealthEvent{} },
		"packet_data":                     func() any { return &envspec.PacketData{} },
		"flow_data":                       func() any { return &envspec.FlowData{} },
		"dns_data":                        func() any { return &envspec.DNSData{} },
		"tls_data":                        func() any { return &envspec.TLSData{} },
		"http_data":                       func() any { return &envspec.HTTPData{} },
		"fs_activity":                     func() any { return &envspec.FSActivityData{} },
		"binder_data":                     func() any { return &envspec.BinderData{} },
		"fs_state_event":                  func() any { return &envspec.FSStateEvent{} },
		"fs_state_params":                 func() any { return &envspec.FSStateParams{} },
		"crash_event":                     func() any { return &envspec.CrashEvent{} },
		"state_event":                     func() any { return &envspec.StateEvent{} },
		"state_params":                    func() any { return &envspec.StateParams{} },
		"memory_event":                    func() any { return &envspec.MemoryEvent{} },
		"memory_params":                   func() any { return &envspec.MemoryParams{} },
		"ipc_event":                       func() any { return &envspec.IPCEvent{} },
		"frida_data":                      func() any { return &envspec.FridaData{} },
		"frida_params":                    func() any { return &envspec.FridaParams{} },
		"process_data":                    func() any { return &envspec.ProcessData{} },
		"signal_event":                    func() any { return &envspec.SignalEvent{} },
		"quality":                         func() any { return &envspec.Quality{} },
		"range_meta":                      func() any { return &envspec.RangeMeta{} },
		"coverage":                        func() any { return &envspec.Coverage{} },
		"selector":                        func() any { return &envspec.Selector{} },
		"stream_state":                    func() any { return &envspec.StreamState{} },
		"lifecycle_event":                 func() any { return &envspec.LifecycleEvent{} },
		"component_state":                 func() any { return &envspec.ComponentState{} },
		"info":                            func() any { return &envspec.Info{} },
		"host_snapshot":                   func() any { return &envspec.HostSnapshot{} },
		"host_capacity":                   func() any { return &envspec.HostCapacity{} },
		"transfer_stats":                  func() any { return &envspec.TransferStats{} },
		"package_mutation":                func() any { return &envspec.PackageMutation{} },
		"package_snapshot":                func() any { return &envspec.PackageSnapshot{} },
		"package_sequence_receipt":        func() any { return &envspec.PackageSequenceReceipt{} },
		"package_action":                  func() any { return &envspec.PackageAction{} },
		"package_action_receipt":          func() any { return &envspec.PackageActionReceipt{} },
		"package_sequence_action_receipt": func() any { return &envspec.PackageSequenceActionReceipt{} },
		"backup_receipt":                  func() any { return &envspec.BackupReceipt{} },
	}
}

func canonical(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	return v
}

func TestGoldenJSONRoundTrip(t *testing.T) {
	cases := goldenCases()
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("golden files: %v (%d files)", err, len(files))
	}
	if len(files) != len(cases) {
		t.Fatalf("golden files (%d) and cases (%d) disagree; every file needs a case and vice versa", len(files), len(cases))
	}
	for _, file := range files {
		name := filepath.Base(file)
		name = name[:len(name)-len(".json")]
		newVal, ok := cases[name]
		if !ok {
			t.Errorf("%s: no case", file)
			continue
		}
		golden, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		v := newVal()
		dec := json.NewDecoder(bytes.NewReader(golden))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: decode into new type: %v", name, err)
			continue
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Errorf("%s: re-marshal: %v", name, err)
			continue
		}
		if want, got := canonical(t, golden), canonical(t, out); !reflect.DeepEqual(want, got) {
			t.Errorf("%s: JSON drift\nwant: %s\ngot:  %s", name, golden, out)
		}
	}
}

// Every stream the predecessor's collector set defined has a name constant
// here (23 streams; net.pcap is a byte stream with no payload struct).
func TestStreamNameCoverage(t *testing.T) {
	got := []string{
		envspec.StreamHostMetrics, envspec.StreamEnvResources, envspec.StreamContainerLogs,
		envspec.StreamEnvProcess, envspec.StreamFSChanges, envspec.StreamFSObserve,
		envspec.StreamFSActivity, envspec.StreamEnvHealth, envspec.StreamAndroidLogcat,
		envspec.StreamAndroidProc, envspec.StreamAndroidPackage, envspec.StreamAndroidCrash,
		envspec.StreamAndroidState, envspec.StreamAndroidMemory, envspec.StreamAndroidBinder,
		envspec.StreamAndroidIPC, envspec.StreamAndroidFrida, envspec.StreamNetPCAP,
		envspec.StreamNetPackets, envspec.StreamNetFlows, envspec.StreamNetDNS,
		envspec.StreamNetTLS, envspec.StreamNetHTTP,
	}
	want := map[string]bool{
		"host.metrics@1": true, "env.resources@1": true, "container.logs@1": true,
		"env.process@1": true, "fs.changes@1": true, "fs.changes@2": true,
		"fs.activity@1": true, "env.health@1": true, "android.logcat@1": true,
		"android.proc@1": true, "android.packages@1": true, "android.crash@2": true,
		"android.state@1": true, "android.memory@1": true, "android.binder@1": true,
		"android.ipc@1": true, "android.frida@1": true, "net.pcap": true,
		"net.packets@1": true, "net.flows@1": true, "net.dns@1": true,
		"net.tls@1": true, "net.http@1": true,
	}
	if len(got) != len(want) {
		t.Fatalf("stream constants: %d, want %d", len(got), len(want))
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected stream name %q", s)
		}
		delete(want, s)
	}
	for s := range want {
		t.Errorf("missing stream constant for %q", s)
	}
}
