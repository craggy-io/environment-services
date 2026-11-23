package envcap

import "encoding/json"

// Input types of the android, signals and rules capabilities.
// Each is the capability's declared input schema (inputSchema) as
// well as what its handler decodes, so the two cannot drift; the
// env.* ones sit beside their handlers in handlers_env.go.

type androidInstallIn struct {
	Env      string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Artifact string `json:"artifact" jsonschema:"APK artifact id"`
	Replace  bool   `json:"replace,omitempty" jsonschema:"reinstall over an existing install, keeping its data"`
}

type androidInputIn struct {
	Env     string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Tap     []int  `json:"tap,omitempty" jsonschema:"[x, y]"`
	Swipe   []int  `json:"swipe,omitempty" jsonschema:"[x1, y1, x2, y2] or [x1, y1, x2, y2, ms]"`
	Keycode *int   `json:"keycode,omitempty" jsonschema:"Android KEYCODE_* number"`
	Text    string `json:"text,omitempty" jsonschema:"text to type into the focused field"`
}

type typeSecretIn struct {
	Env   string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Lease string `json:"lease" jsonschema:"credential lease id (lease_…) from core.credentials.lease, held by env:<env>"`
	Field string `json:"field,omitempty" jsonschema:"secret field to type; password when left out, api_key for an api-key"`
}

type fridaHookIn struct {
	Env                  string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Package              string `json:"package,omitempty" jsonschema:"Android package to attach to or spawn; mutually exclusive with process"`
	Process              string `json:"process,omitempty" jsonschema:"exact running process name to attach to; mutually exclusive with package; cannot spawn"`
	Script               string `json:"script" jsonschema:"Frida JavaScript"`
	Spawn                bool   `json:"spawn,omitempty" jsonschema:"spawn the package instead of attaching to it"`
	CheckpointIntervalMS int    `json:"checkpoint_interval_ms,omitempty" jsonschema:"zero disables; 100..60000 polls the script checkpoint RPC with host timestamps and a 5 second deadline"`
}

type streamsListIn struct {
	Env string `json:"env,omitempty" jsonschema:"environment (env_…) to check each stream against"`
}

type pinIn struct {
	Env    string          `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Stream string          `json:"stream" jsonschema:"one exact stream name from signals.streams.list, e.g. net.flows@1"`
	Params json.RawMessage `json:"params,omitempty" jsonschema:"collector parameters"`
}

type unpinIn struct {
	Pin string `json:"pin" jsonschema:"pin id from signals.pin"`
}

type captureIn struct {
	signalsQueryIn
	Before string `json:"before,omitempty" jsonschema:"window start as a duration before now, e.g. 2m"`
	After  string `json:"after,omitempty" jsonschema:"window end as a duration after now"`
	Name   string `json:"name,omitempty" jsonschema:"name of the capture artifact"`
}

type ruleLoadIn struct {
	Source string         `json:"source" jsonschema:"Starlark rule source"`
	Params map[string]any `json:"params,omitempty"`
}

type ruleUnloadIn struct {
	RuleInstance string `json:"rule_instance" jsonschema:"rule instance id from rules.load"`
}

type ruleTestIn struct {
	Source string         `json:"source" jsonschema:"Starlark rule source"`
	Params map[string]any `json:"params,omitempty"`
	Within string         `json:"within,omitempty" jsonschema:"recorded window to replay, as a duration ending now; default 10m"`
	Env    string         `json:"env,omitempty" jsonschema:"replay only this environment's signals"`
}

type triggersWaitIn struct {
	Rule        string `json:"rule,omitempty" jsonschema:"only this rule's triggers"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"at most 60; default 30"`
}
