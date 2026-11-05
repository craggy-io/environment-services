package envcap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// deviceOf resolves the Android device for an env the caller holds.
func (s *set) deviceOf(ctx context.Context, envID string) (AndroidDevice, error) {
	if _, _, err := s.envOwned(ctx, envID); err != nil {
		return nil, err
	}
	dev, err := s.deps.Manager.Android(envID)
	if err != nil {
		return nil, &agentError{Code: "unavailable",
			Message: fmt.Sprintf("%s is not an Android environment", envID),
			Hint:    `env.provision {"profile":"android"}`}
	}
	return dev, nil
}

func (s *set) buildAndroid() []capability {
	return []capability{
		{Name: "env.android.install", Version: 1, Effects: "long", Tags: []string{"agent"},
			Description: "Install an APK artifact on an Android environment.",
			Input:       inputSchema[androidInstallIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in androidInstallIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				dev, err := s.deviceOf(ctx, in.Env)
				if err != nil {
					return nil, err
				}
				if s.deps.Artifacts == nil || in.Artifact == "" {
					return nil, &agentError{Code: "invalid", Message: "install needs an artifact", Hint: "pass the APK artifact id"}
				}
				op := s.operation(ctx, "env.android.install@1", "", in.Env, []string{in.Artifact})
				_, err = s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
					src, err := run.Source(ctx, in.Artifact)
					if err != nil {
						return err
					}
					apk, err := firstFile(ctx, src)
					if err != nil {
						return err
					}
					return dev.Install(ctx, bytes.NewReader(apk), in.Replace)
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"env": in.Env, "installed": true}, nil
			}},
		{Name: "env.android.screenshot", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Capture the screen; returns a PNG artifact.",
			Input:       inputSchema[envIDIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in envIDIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				dev, err := s.deviceOf(ctx, in.Env)
				if err != nil {
					return nil, err
				}
				png, err := dev.Screenshot(ctx)
				if err != nil {
					return nil, err
				}
				if s.deps.Artifacts == nil {
					return map[string]any{"env": in.Env, "png_base64": base64.StdEncoding.EncodeToString(png)}, nil
				}
				op := s.operation(ctx, "env.android.screenshot@1", "", in.Env, nil)
				refs, err := s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
					_ = run.Record(ctx, Evidence{Environment: in.Env})
					if err := run.Stage(ctx, "screenshot", func(sink envspec.Sink) error {
						return sink.Add("screen.png", 0o100644, bytes.NewReader(png))
					}); err != nil {
						return err
					}
					return run.Keep("screenshot")
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"env": in.Env, "artifact": refs["screenshot"].ID}, nil
			}},
		{Name: "env.android.input", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Send input: {tap:[x,y]} | {swipe:[x1,y1,x2,y2,ms]} | {keycode:n} | {text:\"...\"}.",
			Input:       inputSchema[androidInputIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in androidInputIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				dev, err := s.deviceOf(ctx, in.Env)
				if err != nil {
					return nil, err
				}
				switch {
				case len(in.Tap) == 2:
					err = dev.Tap(ctx, in.Tap[0], in.Tap[1])
				case len(in.Swipe) >= 4:
					ms := 300
					if len(in.Swipe) > 4 {
						ms = in.Swipe[4]
					}
					err = dev.Swipe(ctx, in.Swipe[0], in.Swipe[1], in.Swipe[2], in.Swipe[3], ms)
				case in.Keycode != nil:
					err = dev.KeyEvent(ctx, *in.Keycode)
				case in.Text != "":
					err = dev.Text(ctx, in.Text)
				default:
					return nil, &agentError{Code: "invalid", Message: "give tap, swipe, keycode or text"}
				}
				if err != nil {
					return nil, err
				}
				return map[string]any{"env": in.Env, "sent": true}, nil
			}},
		{Name: "env.android.type-secret", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Type a leased credential field into the focused field. You never see the value.",
			Input:       inputSchema[typeSecretIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in typeSecretIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				if err := requireManager(s); err != nil {
					return nil, err
				}
				if !envspec.HasPrefix(in.Lease, "lease_") {
					return nil, &agentError{Code: "invalid", Message: fmt.Sprintf("%q is not a lease ID (want lease_…)", in.Lease)}
				}
				if _, _, err := s.envOwned(ctx, in.Env); err != nil {
					return nil, err
				}
				if err := s.deps.Manager.TypeSecret(ctx, in.Env, in.Lease, in.Field); err != nil {
					return nil, err
				}
				return map[string]any{"env": in.Env, "typed": in.Field}, nil
			}},
		{Name: "env.android.frida.hook", Version: 1, Effects: "long", Tags: []string{"agent"},
			Description: "Attach/spawn a Frida script (param script = JavaScript). Frida setup is handled for you; output streams to android.frida@1. Returns a collector id.",
			Input:       inputSchema[fridaHookIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in fridaHookIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				params, _ := json.Marshal(map[string]any{"script": in.Script, "package": in.Package, "process": in.Process, "spawn": in.Spawn,
					"checkpoint_interval_ms": in.CheckpointIntervalMS})
				pin, err := s.pinStream(ctx, in.Env, "android.frida@1", params)
				if err != nil {
					return nil, err
				}
				col := ""
				if ids := s.deps.Manager.StreamState("android.frida@1", in.Env, params).Collectors; len(ids) == 1 {
					col = ids[0]
				}
				return map[string]any{"env": in.Env, "collector": col, "stream": "android.frida@1", "pin": pin.ID,
					"hint": "signals.unpin {pin} stops the hook"}, nil
			}},
	}
}

func (s *set) buildVault() []capability {
	return []capability{
		{Name: "vault.creds.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "List credentials (public fields only) with lease availability.",
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				views, err := s.deps.Manager.ListCreds(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"creds": views}, nil
			}},
		{Name: "vault.lease", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Lease a credential by id or selector {kind, labels} for an env/holder. Returns public fields, never secrets.",
			Input:       inputSchema[vaultLeaseIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in vaultLeaseIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				owner, err := s.owner(ctx)
				if err != nil {
					return nil, err
				}
				holder := in.Holder
				holderKind := ""
				if in.Env != "" {
					info, _, err := s.envOwned(ctx, in.Env)
					if err != nil {
						return nil, err
					}
					holder = "env:" + info.ID
					holderKind = "env:" + string(info.Kind)
				}
				if holder == "" {
					holder = owner.ID
				}
				req := LeaseCredRequest{Cred: in.Cred, Kind: in.Kind, Labels: in.Labels,
					Holder: holder, HolderKind: holderKind, Owner: owner.ID, Purpose: in.Purpose}
				if in.TTLSeconds > 0 {
					req.TTL = time.Duration(in.TTLSeconds) * time.Second
				}
				lease, err := s.deps.Manager.LeaseCred(ctx, req)
				if err != nil {
					return nil, err
				}
				return map[string]any{"lease": lease.Lease, "public": lease.Public, "kind": lease.Kind}, nil
			}},
		{Name: "vault.release", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Release a credential lease.",
			Input:       inputSchema[leaseIDIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in leaseIDIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				if err := s.deps.Manager.ReleaseCredLease(ctx, in.Lease); err != nil {
					return nil, err
				}
				return map[string]any{"released": in.Lease}, nil
			}},
		{Name: "vault.creds.put", Version: 1, Effects: "mutating", Tags: []string{"admin"},
			Description: "ADMIN: create/replace a credential. The only way secrets enter.",
			Input:       inputSchema[credPutIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in credPutIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				id, err := s.deps.Manager.PutCred(ctx, CredInput{ID: in.ID, Kind: in.Kind, Name: in.Name,
					Public: in.Public, Secret: in.Secret, Labels: in.Labels, Policy: in.Policy})
				if err != nil {
					return nil, err
				}
				return map[string]any{"cred": id}, nil
			}},
		{Name: "vault.creds.delete", Version: 1, Effects: "mutating", Tags: []string{"admin"},
			Description: "ADMIN: really delete a credential.",
			Input:       inputSchema[credIDIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in credIDIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				if err := s.deps.Manager.DeleteCred(ctx, in.Cred); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": in.Cred}, nil
			}},
		{Name: "vault.creds.policy.set", Version: 1, Effects: "mutating", Tags: []string{"admin"},
			Description: "ADMIN: replace a credential's lease policy.",
			Input:       inputSchema[policySetIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in policySetIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				if err := s.deps.Manager.SetCredPolicy(ctx, in.Cred, in.Policy); err != nil {
					return nil, err
				}
				return map[string]any{"cred": in.Cred, "updated": true}, nil
			}},
	}
}

type signalsQueryIn struct {
	Collectors  []string       `json:"collectors,omitempty" jsonschema:"exact collector instance ids"`
	Streams     []string       `json:"streams,omitempty" jsonschema:"stream names, e.g. android.logcat@1"`
	Sources     []string       `json:"sources,omitempty" jsonschema:"source ids (usually environment ids)"`
	Env         string         `json:"env,omitempty" jsonschema:"shorthand for one environment source (env_…)"`
	Where       map[string]any `json:"where,omitempty" jsonschema:"field equality filter on event data"`
	FromAgo     string         `json:"from_ago,omitempty" jsonschema:"window start as a duration before now, e.g. 2m"`
	Within      string         `json:"within,omitempty" jsonschema:"window as a duration ending now; default 15m"`
	From        string         `json:"from,omitempty" jsonschema:"window start, RFC3339"`
	To          string         `json:"to,omitempty" jsonschema:"window end, RFC3339; default now"`
	Offset      int            `json:"offset,omitempty"`
	Limit       int            `json:"limit,omitempty" jsonschema:"events per page, at most 200"`
	WaitSeconds int            `json:"wait_seconds,omitempty" jsonschema:"signals.tail only: how long to wait, at most 60"`
}

func (in signalsQueryIn) window() (sel envspec.Selector, from, to time.Time) {
	sel = envspec.Selector{Streams: in.Streams, Sources: in.Sources, Collectors: in.Collectors}
	if in.Env != "" {
		sel.Sources = append(sel.Sources, in.Env)
	}
	now := time.Now()
	from, to = time.Time{}, now
	switch {
	case in.From != "":
		if t, err := time.Parse(time.RFC3339, in.From); err == nil {
			from = t
		}
	case in.FromAgo != "":
		if d, err := time.ParseDuration(in.FromAgo); err == nil {
			from = now.Add(-d)
		}
	case in.Within != "":
		if d, err := time.ParseDuration(in.Within); err == nil {
			from = now.Add(-d)
		}
	default:
		from = now.Add(-15 * time.Minute)
	}
	if in.To != "" {
		if t, err := time.Parse(time.RFC3339, in.To); err == nil {
			to = t
		}
	}
	return sel, from, to
}

func eventsOut(evs []envspec.SignalEvent, offset, limit int, q envspec.Quality) map[string]any {
	total := len(evs)
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	page := evs
	if offset > 0 {
		if offset >= len(page) {
			page = nil
		} else {
			page = page[offset:]
		}
	}
	if len(page) > limit {
		page = page[:limit]
	}
	var items []map[string]any
	for _, ev := range page {
		items = append(items, map[string]any{
			"stream": ev.Stream, "source": ev.Source, "seq": ev.Seq,
			"time": ev.Time.Format(time.RFC3339Nano), "data": ev.Data,
		})
	}
	return map[string]any{"total": total, "events": items, "offset": offset,
		"has_more": offset+len(page) < total, "loss_free": q.LossFree(),
		"quality": map[string]any{"dropped": q.Dropped, "seq_gaps": q.SeqGaps, "restarts": q.Restarts, "contains_secret": q.ContainsSecret}}
}

func (s *set) buildSignals() []capability {
	return []capability{
		{Name: "signals.streams.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "Streams available per environment with schema summaries and cost. With env: whether each can run there, whether it is running now, its pins and its last failure. Most streams record nothing until started: signals.pin starts one.",
			Input:       inputSchema[streamsListIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in streamsListIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				var streams []map[string]any
				for _, d := range s.deps.Manager.Streams(in.Env) {
					m := map[string]any{"stream": d.Stream, "description": d.Description, "cost": d.Cost, "bytes": d.Bytes}
					if in.Env != "" {
						if d.Available != nil {
							m["available"] = *d.Available
							if !*d.Available {
								m["why_not"] = d.WhyNot
							}
						}
						m["running"] = d.Running
						if d.LastFailure != "" {
							m["last_failure"] = d.LastFailure
							m["failed_at"] = d.FailedAt.Format(time.RFC3339)
						}
						if len(d.Pins) > 0 {
							m["pins"] = d.Pins
						}
					}
					streams = append(streams, m)
				}
				return map[string]any{"streams": streams}, nil
			}},
		{Name: "signals.query", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "Query recorded signals (what did logcat say before the crash?). Paginated. Only streams whose collector was running were recorded; notes say when a requested stream was not.",
			Input:       inputSchema[signalsQueryIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in signalsQueryIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				sel, from, to := in.window()
				evs, q, err := s.deps.Manager.QuerySignals(sel, in.Where, from, to)
				if err != nil {
					return nil, err
				}
				out := eventsOut(evs, in.Offset, in.Limit, q)
				if notes := s.coverageNotes(sel, from, to, evs); len(notes) > 0 {
					out["notes"] = notes
				}
				return out, nil
			}},
		{Name: "signals.tail", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "Wait up to 60s for new matching events. Tail never starts a collector: signals.pin the stream first, or nothing arrives.",
			Input:       inputSchema[signalsQueryIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in signalsQueryIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				sel, _, _ := in.window()
				wait := in.WaitSeconds
				if wait <= 0 {
					wait = 10
				}
				if wait > 60 {
					wait = 60
				}
				limit := in.Limit
				if limit <= 0 || limit > 200 {
					limit = 50
				}
				got, q, err := s.deps.Manager.TailSignals(ctx, sel, in.Where, time.Duration(wait)*time.Second, limit)
				if err != nil {
					stream := ""
					if len(sel.Streams) == 1 {
						stream = sel.Streams[0]
					}
					return nil, pinErr(in.Env, stream, err)
				}
				return eventsOut(got, 0, limit, q), nil
			}},
		{Name: "signals.pin", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Start recording one stream (e.g. net.flows@1, net.pcap, android.binder@1) on an environment you hold, and keep it recording until signals.unpin or the environment is released. Checks the environment can produce it first and fails with the reason if not. Idempotent per env, stream and params.",
			Input:       inputSchema[pinIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				var in pinIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				if in.Env == "" || in.Stream == "" {
					return nil, &agentError{Code: "invalid", Message: "signals.pin needs env and stream",
						Hint: "signals.streams.list {env} lists the streams"}
				}
				if strings.ContainsAny(in.Stream, "*?") {
					return nil, &agentError{Code: "invalid", Message: "signals.pin takes one exact stream name, not a glob",
						Hint: "pin each stream separately; signals.streams.list {env} lists them"}
				}
				pin, err := s.pinStream(ctx, in.Env, in.Stream, in.Params)
				if err != nil {
					return nil, err
				}
				out := pinView(pin, s.deps.Manager.StreamState(in.Stream, in.Env, pin.Params))
				out["hint"] = "recording from since; signals.query or signals.capture read it, signals.unpin {pin} stops it"
				return out, nil
			}},
		{Name: "signals.unpin", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Stop a signals.pin. What was recorded stays queryable (signals.query) and capturable (signals.capture) while it is retained.",
			Input:       inputSchema[unpinIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in unpinIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				pin, err := s.pinRecord(in.Pin)
				if err != nil {
					return nil, err
				}
				owner, err := s.owner(ctx)
				if err != nil {
					return nil, err
				}
				if pin.Owner != owner.ID {
					if _, _, err := s.envOwned(ctx, pin.Env); err != nil {
						return nil, err
					}
				}
				stopped, err := s.deps.Manager.UnpinStream(pin.ID)
				if err != nil {
					return nil, err
				}
				return map[string]any{"pin": stopped.ID, "env": stopped.Env, "stream": stopped.Stream,
					"since": stopped.Since.Format(time.RFC3339), "until": time.Now().Format(time.RFC3339),
					"hint": "signals.capture {env, streams, from: since, to: until} stores the recording as an artifact"}, nil
			}},
		{Name: "signals.capture", Version: 1, Effects: "long", Tags: []string{"agent"},
			Description: "Store an already-recorded time range of signals (before/after windows OK) as an artifact. It saves what running collectors recorded and starts none: start streams with signals.pin before the activity. Notes say when a requested stream was not being recorded.",
			Input:       inputSchema[captureIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in captureIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				sel, from, to := in.window()
				if in.Before != "" || in.After != "" {
					now := time.Now()
					if d, err := time.ParseDuration(in.Before); err == nil {
						from = now.Add(-d)
					}
					if d, err := time.ParseDuration(in.After); err == nil {
						to = now.Add(d)
					}
				}
				capObj, err := s.deps.Manager.CaptureSignals(sel, in.Where, from, to)
				if err != nil {
					return nil, err
				}
				defer capObj.Close()
				out := map[string]any{"capture": capObj.ID()}
				if evs, _, err := capObj.Events(); err == nil {
					if notes := s.coverageNotes(sel, from, to, evs); len(notes) > 0 {
						out["notes"] = notes
					}
				}
				if s.deps.Artifacts != nil {
					name := in.Name
					if name == "" {
						name = "capture"
					}
					op := s.operation(ctx, "signals.capture@1", "", in.Env, nil)
					refs, err := s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
						_ = run.Record(ctx, Evidence{Range: capObj.ID()})
						if err := stageTree(ctx, run, name, capObj.Source()); err != nil {
							return err
						}
						return run.Keep(name)
					})
					if err != nil {
						return nil, err
					}
					out["artifact"] = refs[name].ID
				} else {
					evs, q, _ := capObj.Events()
					out["events"] = len(evs)
					out["loss_free"] = q.LossFree()
				}
				return out, nil
			}},
	}
}

// pinRecord looks a pin up by id. Pins("") is the manager's
// all-pins listing; ownership is checked against the record.
func (s *set) pinRecord(pinID string) (PinRecord, error) {
	for _, p := range s.deps.Manager.Pins("") {
		if p.ID == pinID {
			return p, nil
		}
	}
	return PinRecord{}, &agentError{Code: "not_found",
		Message: fmt.Sprintf("no pin %q (already unpinned, or its environment was released)", pinID),
		Hint:    "signals.streams.list {env} shows the pins on an environment"}
}

func (s *set) buildRules() []capability {
	return []capability{
		{Name: "rules.load", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Load (or hot-reload) a Starlark signal rule from source text with params.",
			Input:       inputSchema[ruleLoadIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in ruleLoadIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				// The host stores the source as an artifact first;
				// without a bridge a content digest is the code ref.
				codeRef := "sha256:rule-" + shortDigest(in.Source)
				if s.deps.Artifacts != nil {
					op := s.operation(ctx, "rules.load@1", "", "", nil)
					refs, _ := s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
						if err := run.Stage(ctx, "rule-source", func(sink envspec.Sink) error {
							return sink.Add("rule.star", 0o100644, strings.NewReader(in.Source))
						}); err != nil {
							return err
						}
						return run.Keep("rule-source")
					})
					if ref, ok := refs["rule-source"]; ok && ref.ID != "" {
						codeRef = ref.ID
					}
				}
				id, err := s.deps.Manager.LoadRule(ctx, []byte(in.Source), codeRef, in.Params)
				if err != nil {
					return nil, &agentError{Code: "invalid", Message: err.Error(), Hint: "fix the rule and load again; the running version was left in place"}
				}
				return map[string]any{"rule_instance": id, "code_ref": codeRef}, nil
			}},
		{Name: "rules.unload", Version: 1, Effects: "mutating", Tags: []string{"agent"},
			Description: "Stop a rule instance. Idempotent.",
			Input:       inputSchema[ruleUnloadIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in ruleUnloadIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				_ = s.deps.Manager.UnloadRule(in.RuleInstance)
				return map[string]any{"unloaded": in.RuleInstance}, nil
			}},
		{Name: "rules.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "List rule instances with counters and last triggers.",
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var out []map[string]any
				for _, ri := range s.deps.Manager.ListRules() {
					m := map[string]any{"instance": ri.Instance, "rule": ri.Rule, "version": ri.Version,
						"description": ri.Description, "code_ref": ri.CodeRef, "state": ri.State,
						"events": ri.Events, "emits": ri.Emits, "triggers": ri.Triggers, "faults": ri.Faults}
					if !ri.LoadedAt.IsZero() {
						m["loaded_at"] = ri.LoadedAt.Format(time.RFC3339)
					}
					if !ri.LastTrigger.IsZero() {
						m["last_trigger"] = ri.LastTrigger.Format(time.RFC3339)
					}
					out = append(out, m)
				}
				return map[string]any{"rules": out}, nil
			}},
		{Name: "rules.test", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "Backtest a rule against a recorded window (default last 10m) without publishing anything.",
			Input:       inputSchema[ruleTestIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in ruleTestIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				within := 10 * time.Minute
				if in.Within != "" {
					if d, err := time.ParseDuration(in.Within); err == nil {
						within = d
					}
				}
				sel := envspec.Selector{}
				if in.Env != "" {
					sel.Sources = []string{in.Env}
				}
				now := time.Now()
				evs, _, err := s.deps.Manager.QuerySignals(sel, nil, now.Add(-within), now)
				if err != nil {
					return nil, err
				}
				res, err := s.deps.Manager.TestRule(ctx, []byte(in.Source), in.Params, evs)
				if err != nil {
					return nil, &agentError{Code: "invalid", Message: err.Error()}
				}
				var trigs []map[string]any
				for _, t := range res.Triggers {
					trigs = append(trigs, map[string]any{"kind": t.Kind, "time": t.Time.Format(time.RFC3339), "data": t.Data})
				}
				return map[string]any{"rule": res.Rule, "emits": res.Emits, "triggers": trigs, "faults": res.Faults, "replayed": len(evs)}, nil
			}},
		{Name: "rules.triggers.wait", Version: 1, Effects: "readonly", Tags: []string{"agent"},
			Description: "Wait for your rules to trigger (up to 60s).",
			Input:       inputSchema[triggersWaitIn](),
			Handler: func(ctx context.Context, call *call) (any, error) {
				if err := requireManager(s); err != nil {
					return nil, err
				}
				var in triggersWaitIn
				if err := decode(call, &in); err != nil {
					return nil, err
				}
				wait := in.WaitSeconds
				if wait <= 0 {
					wait = 30
				}
				if wait > 60 {
					wait = 60
				}
				ts, err := s.deps.Manager.WaitRuleTriggers(ctx, in.Rule, time.Duration(wait)*time.Second)
				if err != nil {
					return nil, err
				}
				var out []map[string]any
				for _, t := range ts {
					out = append(out, map[string]any{"rule": t.Rule, "kind": t.Kind, "time": t.Time.Format(time.RFC3339),
						"data": t.Data, "capture": t.CaptureID})
				}
				return map[string]any{"triggers": out}, nil
			}},
	}
}
