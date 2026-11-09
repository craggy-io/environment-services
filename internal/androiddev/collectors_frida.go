package androiddev

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed assets/frida_session.py
var fridaSession string

// --- android.frida@1 ---

// AndroidFrida runs a Frida agent script through the host frida client
// against the managed frida-server component. The agent never handles
// frida-server, ports or versions.
type fridaCollector struct{ dev *Device }

func (f *fridaCollector) Name() string { return "android.frida" }
func (f *fridaCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamAndroidFrida, Description: "Frida script send() events", Cost: "high",
		Schema: signalcore.SchemaFor(envspec.FridaData{})}}
}
func (f *fridaCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamAndroidFrida, "android")
	}
	if !e.Root {
		return fmt.Errorf("frida needs root; %s uses profile %s. hint: env.provision {profile: \"android\"}", e.ID, e.Profile)
	}
	return nil
}

// Preflight checks the host side of Frida: the frida client the
// collector drives. The device-side frida-server is a managed component,
// health-checked when the collector starts.
func (f *fridaCollector) Preflight(ctx context.Context) error {
	_, err := fridaPython(ctx)
	return err
}

func (f *fridaCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{
		"script":                 strSchema(),
		"package":                strSchema(),
		"process":                strSchema(),
		"spawn":                  {Type: "boolean"},
		"port":                   {Type: "integer"},
		"checkpoint_interval_ms": {Type: "integer"},
	}, "script")
}

type fridaParams struct {
	Script               string `json:"script"`
	Package              string `json:"package"`
	Process              string `json:"process,omitempty"`
	Spawn                bool   `json:"spawn"`
	Port                 int    `json:"port"`
	Address              string `json:"address"`
	CheckpointIntervalMS int    `json:"checkpoint_interval_ms,omitempty"`
}

func (p fridaParams) validate() error {
	if strings.TrimSpace(p.Script) == "" {
		return fmt.Errorf("android.frida: script is required")
	}
	if (strings.TrimSpace(p.Package) == "") == (strings.TrimSpace(p.Process) == "") {
		return fmt.Errorf("android.frida: exactly one of package or process is required")
	}
	if p.Spawn && p.Process != "" {
		return fmt.Errorf("android.frida: spawn requires package; process only attaches to an existing exact process name")
	}
	if p.Port < 0 || p.Port > 65535 {
		return fmt.Errorf("android.frida: invalid server port")
	}
	if p.CheckpointIntervalMS != 0 && (p.CheckpointIntervalMS < 100 || p.CheckpointIntervalMS > 60000) {
		return fmt.Errorf("android.frida: checkpoint_interval_ms must be zero or 100..60000")
	}
	return nil
}

func fridaPython(ctx context.Context) (string, error) {
	for _, name := range []string{"python", "python3"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = exec.CommandContext(probe, bin, "-c", "import frida, frida_tools").Run()
		cancel()
		if err == nil {
			return bin, nil
		}
	}
	return "", fmt.Errorf("android.frida: Python with frida and frida-tools is required on the host")
}

func (f *fridaCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := f.dev
	var p fridaParams
	if err := json.Unmarshal(params, &p); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	if p.Port == 0 {
		p.Port = 27042
	}
	python, err := fridaPython(ctx)
	if err != nil {
		return err
	}
	if err := e.EnsureComponent(ctx, "frida"); err != nil {
		return err
	}
	client, serial := e.tr, e.cfg.Serial
	port, err := client.Forward(ctx, serial, 0, p.Port)
	if err != nil {
		return fmt.Errorf("android.frida: adb forward: %w", err)
	}
	e.noteForward(port)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.Forward(cleanup, serial, port, 0)
		e.dropForward(port)
	}()
	p.Address = fmt.Sprintf("127.0.0.1:%d", port)
	return runFridaSession(ctx, python, p, out)
}

func runFridaSession(ctx context.Context, python string, p fridaParams, out signalcore.Emitter) error {
	cmd := exec.CommandContext(ctx, python, "-u", "-c", fridaSession)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	// EOF requests detach; WaitDelay bounds cleanup if the SDK hangs.
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := json.NewEncoder(stdin).Encode(p); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	streamErr := readFridaSession(stdout, out)
	if streamErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if streamErr != nil {
		return streamErr
	}
	if waitErr != nil {
		return fmt.Errorf("android.frida: session exited: %w (%s)", waitErr, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("android.frida: session ended before cancellation")
}

func readFridaSession(r io.Reader, out signalcore.Emitter) error {
	var failure error
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var data envspec.FridaData
		if err := json.Unmarshal(sc.Bytes(), &data); err != nil {
			return fmt.Errorf("android.frida: invalid transport record: %w", err)
		}
		switch data.Kind {
		case "attached", "loaded", "message", "log", "collector_error":
		case "checkpoint":
			from, fromErr := time.Parse(time.RFC3339Nano, data.RequestedAt)
			to, toErr := time.Parse(time.RFC3339Nano, data.CompletedAt)
			if fromErr != nil || toErr != nil || to.Before(from) {
				return fmt.Errorf("android.frida: invalid checkpoint host timestamps")
			}
		default:
			return fmt.Errorf("android.frida: unknown transport record %q", data.Kind)
		}
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidFrida, Data: data})
		if data.Kind == "collector_error" {
			// Drain through EOF so the helper can detach and kill an unresumed
			// spawn before the caller reaps it. A failure never becomes silence.
			failure = fmt.Errorf("android.frida: %s", data.Error)
		}
	}
	return errors.Join(failure, sc.Err())
}
