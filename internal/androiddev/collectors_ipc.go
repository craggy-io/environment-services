package androiddev

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// --- android.ipc@1: intent dispatch with caller and resolved target ---
//
// android.binder@1 streams binder metadata with no payload. android.ipc@1
// reads ActivityManager's intent dispatch from logcat — the caller uid,
// the resolved component, the action, the data URI and the URI-grant
// flags of each started activity, started/bound service and ordered
// broadcast. It is receipt-grade over what logcat records: the resolved
// destination and caller, which is what the IPC gates (gate-waves §Wave
// 1, rows 3-5: intent redirection, permission redelegation, PendingIntent
// retarget) read to see an external caller reach, or make the target
// reach, a component. The extras payload and nested-intent bodies are not
// in logcat; a Frida-backed payload stream is the future refinement.
//
// The ActivityManager/ActivityTaskManager log format varies across Android releases; the
// parser takes what it can from each line and leaves the rest. The
// activity path is live-tested on Android 14. Service/broadcast metadata
// remains best-effort: callers must treat missing identity fields as unknown.

const ipcStream = envspec.StreamAndroidIPC

// AndroidIPC streams android.ipc@1 by parsing ActivityManager logcat.
type ipcCollector struct{ dev *Device }

func (a *ipcCollector) Name() string { return "android.ipc" }
func (a *ipcCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: ipcStream, Description: "Intent dispatch (caller, resolved component, action, data, grant flags) from ActivityManager", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.IPCEvent{})}}
}
func (a *ipcCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(ipcStream, "android")
	}
	return nil
}
func (a *ipcCollector) Params() *jsonschema.Schema { return nil }

func (a *ipcCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	// Activity starts moved to ActivityTaskManager on newer Android releases.
	// Both managers log in system_server; reject app-authored lookalike tags.
	cmd := `pid=$(pidof system_server); test -n "$pid" || exit 1; logcat --pid="$pid" -v tag -b main -b system -T 1 -s ActivityManager:I ActivityTaskManager:I '*:S'`
	return guestExecLines(ctx, e, cmd, func(line string) {
		if ev, ok := parseIPCLine(line); ok {
			out.Emit(envspec.SignalEvent{Stream: ipcStream, Data: ev})
		}
	})
}

var (
	ipcStart     = regexp.MustCompile(`START u\d+ (\{.*?\})`)
	ipcCaller    = regexp.MustCompile(`\bfrom uid (\d+)\b`)
	ipcService   = regexp.MustCompile(`Start(?:ing| proc).*?service.*?(\{.*?\})`)
	ipcBroadcast = regexp.MustCompile(`broadcast.*?(\{.*?\})`)
	ipcField     = regexp.MustCompile(`(\bact|\bdat|\btyp|\bflg|\bcmp)=([^ \}]+)`)
)

// parseIPCLine reads one ActivityManager logcat line into an envspec.IPCEvent.
// It recognises activity START, service start and broadcast dispatch;
// anything else returns ok false.
func parseIPCLine(line string) (envspec.IPCEvent, bool) {
	var ev envspec.IPCEvent
	ev.CallerUID = -1
	var brace string
	switch {
	case ipcStart.MatchString(line):
		m := ipcStart.FindStringSubmatch(line)
		brace, ev.Kind = m[1], envspec.IPCActivity
	case ipcService.MatchString(line):
		ev.Kind, brace = envspec.IPCService, ipcService.FindStringSubmatch(line)[1]
	case ipcBroadcast.MatchString(line):
		ev.Kind, brace = envspec.IPCBroadcast, ipcBroadcast.FindStringSubmatch(line)[1]
	default:
		return ev, false
	}
	// Caller metadata must be outside the Intent body, whose strings may
	// be supplied by the app. Missing metadata remains explicitly unknown.
	_, suffix, _ := strings.Cut(line, brace)
	if m := ipcCaller.FindStringSubmatch(suffix); m != nil {
		ev.CallerUID, _ = strconv.Atoi(m[1])
	}
	for _, f := range ipcField.FindAllStringSubmatch(brace, -1) {
		switch f[1] {
		case "act":
			ev.Action = f[2]
		case "dat":
			ev.Data = f[2]
		case "typ":
			ev.Type = f[2]
		case "flg":
			ev.Flags = f[2]
			if n, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32); err == nil {
				ev.GrantRead, ev.GrantWrite = n&0x1 != 0, n&0x2 != 0
			}
		case "cmp":
			ev.Component = f[2]
		}
	}
	if ev.Component == "" && ev.Action == "" {
		return ev, false
	}
	ev.Raw = strings.TrimSpace(line)
	return ev, true
}
