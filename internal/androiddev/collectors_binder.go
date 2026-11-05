package androiddev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// binderCollector streams ftrace binder_transaction events (root).
type binderCollector struct{ dev *Device }

func (b *binderCollector) Name() string { return "android.binder" }
func (b *binderCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamAndroidBinder, Description: "Binder transactions via ftrace (no payload)", Cost: "med",
		Schema: signalcore.SchemaFor(envspec.BinderData{})}}
}
func (b *binderCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamAndroidBinder, "android")
	}
	if !e.Root {
		return rootHint(envspec.StreamAndroidBinder, e.Profile)
	}
	return nil
}

// Preflight checks the guest can trace binder: a root shell, tracefs
// and the binder tracepoints.
func (b *binderCollector) Preflight(ctx context.Context) error {
	return probeScript(ctx, b.dev, "root", rootCheck+`TR=/sys/kernel/tracing; [ -d $TR/events ] || TR=/sys/kernel/debug/tracing
if [ ! -d $TR/events ]; then echo "tracefs is not mounted at /sys/kernel/tracing or /sys/kernel/debug/tracing"
elif [ ! -d $TR/events/binder/binder_transaction ]; then echo "this kernel has no binder tracepoints ($TR/events/binder/binder_transaction is missing)"
elif [ ! -r $TR/trace_pipe ]; then echo "$TR/trace_pipe is not readable as root"
else echo ok; fi`)
}

func (b *binderCollector) Params() *jsonschema.Schema { return nil }
func (b *binderCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	script := "TR=/sys/kernel/tracing; [ -d $TR ] || TR=/sys/kernel/debug/tracing; " +
		"echo 1 > $TR/events/binder/binder_transaction/enable 2>/dev/null; " +
		"echo 1 > $TR/events/binder/binder_transaction_received/enable 2>/dev/null; " +
		"cat $TR/trace_pipe"
	err := guestExecLinesRoot(ctx, b.dev, script, func(line string) {
		if !strings.Contains(line, "binder_transaction") {
			return
		}
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidBinder, Data: parseBinderLine(line)})
	})
	if ctx.Err() != nil {
		return nil
	}
	// trace_pipe never ends on its own; an exit means tracing died,
	// and returning nil would leave a dead collector counted running.
	if err == nil {
		err = errors.New("trace_pipe closed")
	}
	return fmt.Errorf("android.binder: %w", err)
}

// parseBinderLine extracts fields from an ftrace binder line. Format:
// task-pid [cpu] flags ts: binder_transaction: transaction=.. dest_node=..
// dest_proc=pid dest_thread=tid reply=0 flags=0x.. code=0x..
func parseBinderLine(line string) envspec.BinderData {
	d := envspec.BinderData{Raw: ""}
	get := func(key string) string {
		i := strings.Index(line, key+"=")
		if i < 0 {
			return ""
		}
		rest := line[i+len(key)+1:]
		if j := strings.IndexAny(rest, " \t"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	d.PID, _ = strconv.Atoi(get("dest_proc"))
	d.TID, _ = strconv.Atoi(get("dest_thread"))
	d.Target = get("dest_node")
	if v := get("code"); v != "" {
		n, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
		d.Code = uint32(n)
	}
	if v := get("flags"); v != "" {
		n, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
		d.Flags = uint32(n)
	}
	d.Reply = get("reply") == "1" || strings.Contains(line, "binder_transaction_received")
	if d.PID == 0 {
		// Fall back to the leading task-pid.
		f := strings.Fields(line)
		if len(f) > 0 {
			if i := strings.LastIndex(f[0], "-"); i >= 0 {
				d.PID, _ = strconv.Atoi(f[0][i+1:])
			}
		}
	}
	return d
}

// StockBinderHookScript is the stock Frida binder-payload hook.
const StockBinderHookScript = `Java.perform(function() {
  var IBinder = Java.use('android.os.BinderProxy');
  IBinder.transact.implementation = function(code, data, reply, flags) {
    var r = this.transact(code, data, reply, flags);
    send({kind: 'binder', code: code, flags: flags});
    return r;
  };
  send({kind: 'ready', hook: 'BinderProxy.transact'});
});`
