# Rules

Rules are long-lived Starlark programs that watch recorded signals and
announce what they notice: they emit derived signals, raise triggers and pin
evidence captures. Rules never provision, exec or call capabilities — they
observe and announce. The engine lives in `internal/rules` on top of
`github.com/craggy-io/script-services`; hosts reach it through the
`rules.*` capabilities (see [capabilities](capabilities.md)).

## A rule

```python
rule(
    name = "egress-spike",
    version = 1,
    description = "Outbound bytes over 10s exceed 5x the trailing average",
    params = {"env": str, "min_bytes": int},
)
win  = window("10s")
base = ewma(half_life = "5m")

def on_flow(ev):
    win.add(ev.time, ev.data.bytes)

def check():
    total = win.sum()
    if total > params.min_bytes and total > 5 * base.value():
        trigger("spike", {"bytes": total, "baseline": base.value()},
                capture = {"streams": ["net.pcap", "net.flows@1"],
                           "before": "60s", "after": "30s"})

on("net.flows@1", source = params.env, where = {"direction": "out"}, fn = on_flow)
every("10s", check)
```

Predeclared names: `rule` (declare the rule and its typed `params`), `on`
(subscribe; `source` and `where` filters), `every` (timer), `emit` (publish a
derived signal), `trigger` (raise a fact; optional `capture` pins a signal
range around it), `log`, the state helpers `window`, `ewma`, `counter`,
`quantiles`, `distinct`, `dedupe`, plus `state` (a mutable dict that lives
for the instance's whole life), `params`, and the read-only lookups
`env.info(id)` and `host.snapshot()`.

Events arrive as `ev` with `ev.stream`, `ev.source`, `ev.time` (epoch
seconds), `ev.seq` and `ev.data.<field>` attribute access over the payload.

## Semantics and limits

- One goroutine and one 4096-entry mailbox per instance. Events a rule
  cannot keep up with are counted as loss, not queued unboundedly.
- Each event/timer callback runs under a budget of 1,000,000 steps and
  250 ms wall clock. Repeated faults disable the instance (fault-disable)
  rather than wedging the engine.
- Derived signals carry a depth; anything at depth ≥ 4 is dropped, so rules
  cannot loop through each other. Derived streams are marked as such and
  cannot forge collector streams.
- `load()` is rejected at load time. Rules are single, self-contained
  sources.
- Hot reload: loading a new version under the same identity replaces the
  running instance.
- Values convert through script-services `values.FromGo/ToGo`: integral
  floats become Starlark ints (and Starlark ints return to Go as `int64`,
  which `where` matching handles — regression-tested), `[]byte` payloads
  arrive as base64 strings. `while` and top-level control flow are allowed;
  globals are never frozen, so top-level lists/dicts and `state` stay
  mutable across events.
- Known gap, carried knowingly: the `state` dict has no enforced size cap
  (a raw Starlark dict cannot enforce one).

## Host operations

| Operation | Capability | Notes |
| --- | --- | --- |
| Load/replace an instance | `rules.load` | Source + params; returns the instance identity |
| Unload | `rules.unload` | |
| List instances and recent triggers | `rules.list` | Includes disabled/faulted state |
| Backtest | `rules.test` | Replays recorded events under a virtual clock without side effects; reports triggers/emits |
| Wait for a trigger | `rules.triggers.wait` | Blocks until a matching trigger (or timeout) |

`rules.test` is the development loop: iterate on a rule against captures
already recorded, then `rules.load` the result.
