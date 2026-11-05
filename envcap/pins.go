package envcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// pinStream pins one stream on an environment the caller holds and
// maps every refusal to an agent error that says what to change.
// The manager resolves and preflights the collector synchronously
// and waits for its running ack, so a pin that returns is recording.
func (s *set) pinStream(ctx context.Context, envID, stream string, params json.RawMessage) (PinRecord, error) {
	_, owner, err := s.envOwned(ctx, envID)
	if err != nil {
		return PinRecord{}, err
	}
	pin, err := s.deps.Manager.PinStream(ctx, owner.ID, envID, stream, params)
	if err != nil {
		return PinRecord{}, pinErr(envID, stream, err)
	}
	return pin, nil
}

// pinErr maps a pin request error to the agent shape.
func pinErr(envID, stream string, err error) error {
	switch {
	case errors.Is(err, ErrNoStream):
		return &agentError{Code: "invalid", Message: fmt.Sprintf("no collector provides stream %q", stream),
			Hint: "signals.streams.list {env} lists the streams and which ones this environment can run"}
	case errors.Is(err, ErrNoTarget):
		return &agentError{Code: "unavailable", Message: fmt.Sprintf("%s is not running, so nothing can be collected from it", envID),
			Hint: "env.get {env} shows its state; signals start once it is ready"}
	case errors.Is(err, ErrNotApplicable):
		return &agentError{Code: "unavailable", Message: trimStack(err.Error()),
			Hint: "signals.streams.list {env} shows which streams this environment can run; root-only streams need a rooted profile"}
	case errors.Is(err, ErrPreflight):
		return &agentError{Code: "unavailable", Message: "host check failed: " + trimStack(err.Error()),
			Hint: "this environment cannot produce the stream as provisioned; choose another stream or provision a profile that provides it"}
	}
	return err
}

// pinView is the agent-facing shape of one pin.
func pinView(p PinRecord, st envspec.StreamState) map[string]any {
	m := map[string]any{"pin": p.ID, "env": p.Env, "stream": p.Stream,
		"since": p.Since.Format(time.RFC3339), "running": st.Running, "collectors": st.Collectors}
	if st.LastFailure != "" && !st.FailedAt.Before(p.Since) {
		m["last_failure"] = st.LastFailure
		m["failed_at"] = st.FailedAt.Format(time.RFC3339)
	}
	return m
}

// coverageNotes explains why requested streams are empty or
// incomplete: a stream whose collector was not running records
// nothing, so an empty answer there is unknown, not quiet.
func (s *set) coverageNotes(sel envspec.Selector, from, to time.Time, evs []envspec.SignalEvent) []string {
	counts := map[[2]string]int{}
	for _, ev := range evs {
		counts[[2]string{ev.Stream, ev.Source}]++
	}
	var notes []string
	for _, c := range s.deps.Manager.SignalCoverage(sel, from, to) {
		n := counts[[2]string{c.Stream, c.Source}]
		subject := c.Stream + " on " + c.Source
		var note string
		switch {
		case n == 0 && c.Status == "not_running":
			note = fmt.Sprintf("%s: no collector was running for this window, so nothing was recorded; an empty result says nothing about activity.", subject)
		case n == 0 && c.Status == "partial":
			note = fmt.Sprintf("%s: the collector ran for only %s of the %s window and recorded nothing; outside that, nothing was collected.",
				subject, c.Ran.Round(time.Second), c.Window.Round(time.Second))
		}
		if c.Failure != "" {
			if note == "" {
				note = subject + ":"
			}
			note += " Its collector failed during the window: " + c.Failure + "."
		}
		if note == "" {
			continue
		}
		if c.NotRunnable != "" {
			note += " It cannot run there now: " + strings.TrimSpace(c.NotRunnable) + "."
		} else if c.Status != "covered" {
			note += fmt.Sprintf(` Start it before the activity you want to observe: signals.pin {"env":%q,"stream":%q}.`, c.Source, c.Stream)
		}
		notes = append(notes, note)
	}
	return notes
}
