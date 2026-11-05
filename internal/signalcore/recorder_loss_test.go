package signalcore_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

func TestRecorderEvictionLossIsExplicit(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"memory", "spill-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := ""
			if mode == "spill-failure" {
				dir = filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(dir, []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir, RingBytes: 1})
			hub := signalcore.NewHub(signalcore.WithRecorder(rec))
			defer hub.Close()
			hub.Publish(envspec.SignalEvent{Source: "device", Stream: "effect", Collector: "observer", Seq: 1, Time: base, Data: "effect occurred"})
			events, quality, err := rec.Query(envspec.Selector{Streams: []string{"effect"}}, base, base.Add(time.Second))
			if err == nil && quality.LossFree() {
				t.Fatalf("%s silently discarded evidence: events=%v quality=%+v", mode, events, quality)
			}
			capture, err := rec.Capture(envspec.Selector{Streams: []string{"effect"}}, base, base.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Close()
			source := capture.Source()
			found := false
			for entry, err := range source.Entries(context.Background()) {
				if err != nil {
					t.Fatal(err)
				}
				if entry.Path != "range.json" {
					continue
				}
				reader, err := source.Open(context.Background(), entry)
				if err != nil {
					t.Fatal(err)
				}
				var metadata envspec.RangeMeta
				err = json.NewDecoder(reader).Decode(&metadata)
				reader.Close()
				if err != nil || metadata.Quality.LossFree() {
					t.Fatalf("export discarded loss: %+v %v", metadata, err)
				}
				found = true
			}
			if !found {
				t.Fatal("capture omitted range metadata")
			}
		})
	}
}

func TestRecorderSpillNamesCannotCollideAcrossCollectors(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir, RingBytes: 1})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	defer hub.Close()
	for _, collector := range []string{"one", "two"} {
		hub.Publish(envspec.SignalEvent{Source: "device", Stream: "effect", Collector: collector, Seq: 1, Time: base, Data: collector})
	}
	events, quality, err := rec.Query(envspec.Selector{Streams: []string{"effect"}}, base, base.Add(time.Second))
	if err != nil || len(events) != 2 || !quality.LossFree() {
		t.Fatalf("colliding spill files lost evidence: events=%v quality=%+v err=%v", events, quality, err)
	}
	reopened := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir})
	events, quality, err = reopened.Query(envspec.Selector{Streams: []string{"effect"}}, base, base.Add(time.Second))
	if err != nil || len(events) != 2 || !quality.LossFree() {
		t.Fatalf("reloaded spill inventory: events=%v quality=%+v err=%v", events, quality, err)
	}
}

func TestRecorderUnencodableDataIsExplicit(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: t.TempDir(), RingBytes: 1})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	defer hub.Close()
	hub.Publish(envspec.SignalEvent{Source: "device", Stream: "effect", Collector: "observer", Seq: 1, Time: base, Data: map[string]any{"effect": make(chan int)}})
	events, quality, err := rec.Query(envspec.Selector{Streams: []string{"effect"}}, base, base.Add(time.Second))
	if err == nil && quality.LossFree() {
		t.Fatalf("unencodable effect silently became empty payload: events=%v quality=%+v", events, quality)
	}
}

func TestRecorderLossScopedToCollectorAndOverlappingTime(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{RingBytes: 1})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	defer hub.Close()
	for i := 1; i <= 3; i++ {
		hub.Publish(envspec.SignalEvent{Source: "device", Stream: "effect", Collector: "observer", Seq: uint64(i), Time: base.Add(time.Duration(i) * time.Second), Data: strings.Repeat("x", 128)})
	}
	for name, sel := range map[string]envspec.Selector{
		"selected": {Streams: []string{"effect"}, Collectors: []string{"observer"}},
		"other":    {Streams: []string{"effect"}, Collectors: []string{"other"}},
	} {
		_, quality, err := rec.Query(sel, base.Add(1500*time.Millisecond), base.Add(2500*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if quality.LossFree() != (name == "other") {
			t.Fatalf("%s: incorrect overlapping/scoped loss: %+v", name, quality)
		}
	}
}

func TestRecorderSpillIndexesOutOfOrderEvents(t *testing.T) {
	dir := t.TempDir()
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	defer hub.Close()
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	for i, offset := range []int{2, 0, 3, 1} {
		hub.Publish(envspec.SignalEvent{Source: "device", Stream: "effect", Collector: "observer", Seq: uint64(i + 1), Time: base.Add(time.Duration(offset) * time.Second), Data: offset})
	}
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	reopened := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir})
	for offset := range 4 {
		at := base.Add(time.Duration(offset) * time.Second)
		events, quality, err := reopened.Query(envspec.Selector{Streams: []string{"effect"}}, at, at)
		if err != nil || len(events) != 1 || !quality.LossFree() {
			t.Errorf("offset %d silently omitted: events=%v quality=%+v err=%v", offset, events, quality, err)
		}
	}
}
