package signalcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"sort"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// Capture pins a recorded range so retention cannot evict it. Its
// exported form is an envspec.Source: segment files per stream, a
// concatenated pcap for byte streams, and a range.json. Keeping a future
// range fed is a collector activity owned by core; a Capture only reads
// and pins what the recorder holds.
type Capture struct {
	id   string
	rec  *Recorder
	sel  envspec.Selector
	from time.Time
	to   time.Time
}

// ID returns the capture ID (cap_…).
func (c *Capture) ID() string { return c.id }

// Range returns the captured [from,to] bounds.
func (c *Capture) Range() (time.Time, time.Time) { return c.from, c.to }

// Selector returns the capture selector.
func (c *Capture) Selector() envspec.Selector { return c.sel }

// Capture pins [from,to] for sel in the recorder's retention index.
// Source/Events read whatever has been recorded when called.
func (r *Recorder) Capture(sel envspec.Selector, from, to time.Time) (*Capture, error) {
	c := &Capture{id: envspec.NewID("cap_"), rec: r, sel: sel, from: from, to: to}
	r.mu.Lock()
	r.pinned[c.id] = c
	r.mu.Unlock()
	return c, nil
}

// Close releases the retention pin.
func (c *Capture) Close() {
	c.rec.mu.Lock()
	delete(c.rec.pinned, c.id)
	c.rec.mu.Unlock()
}

// Events returns the captured events and their quality.
func (c *Capture) Events() ([]envspec.SignalEvent, envspec.Quality, error) {
	return c.rec.collect(c.sel, c.from, c.to)
}

// Quality reports the captured range's quality.
func (c *Capture) Quality() (envspec.Quality, error) {
	_, q, err := c.Events()
	return q, err
}

// Meta returns the range metadata written to range.json.
func (c *Capture) Meta() (envspec.RangeMeta, error) {
	q, err := c.Quality()
	if err != nil {
		return envspec.RangeMeta{}, err
	}
	return c.metadata(q), nil
}

func (c *Capture) metadata(q envspec.Quality) envspec.RangeMeta {
	return envspec.RangeMeta{ID: c.id, Streams: c.sel.Streams, Sources: c.sel.Sources, Collectors: c.sel.Collectors,
		From: c.from, To: c.to, Quality: q}
}

// Source exports the capture as an envspec.Source tree:
//
//	range.json
//	<source>/<stream>.jsonl   (event streams)
//	<source>/<stream>.pcap    (byte streams, chunks concatenated)
func (c *Capture) Source() envspec.Source { return captureSource{c: c} }

type captureSource struct{ c *Capture }

func (s captureSource) files() (map[string][]byte, error) {
	evs, q, err := s.c.Events()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	meta := s.c.metadata(q)
	mb, _ := json.MarshalIndent(meta, "", "  ")
	files["range.json"] = mb
	byKey := map[string][]envspec.SignalEvent{}
	for _, ev := range evs {
		byKey[ev.Source+"/"+ev.Stream] = append(byKey[ev.Source+"/"+ev.Stream], ev)
	}
	for key, list := range byKey {
		isBytes := false
		for _, ev := range list {
			if _, ok := ev.Data.([]byte); ok {
				isBytes = true
			}
		}
		if isBytes {
			var buf bytes.Buffer
			for _, ev := range list {
				if b, ok := ev.Data.([]byte); ok {
					buf.Write(b)
				}
			}
			files[key+".pcap"] = buf.Bytes()
			continue
		}
		var buf bytes.Buffer
		for _, ev := range list {
			se, err := marshalEvent(ev, false)
			if err != nil {
				return nil, err
			}
			buf.Write(se.raw)
			buf.WriteByte('\n')
		}
		files[key+".jsonl"] = buf.Bytes()
	}
	return files, nil
}

func (s captureSource) Entries(ctx context.Context) iter.Seq2[envspec.Entry, error] {
	return func(yield func(envspec.Entry, error) bool) {
		files, err := s.files()
		if err != nil {
			yield(envspec.Entry{}, err)
			return
		}
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !yield(envspec.Entry{Path: n, Mode: 0o100644, Size: int64(len(files[n]))}, nil) {
				return
			}
		}
	}
}

func (s captureSource) Open(ctx context.Context, e envspec.Entry) (io.ReadCloser, error) {
	files, err := s.files()
	if err != nil {
		return nil, err
	}
	b, ok := files[e.Path]
	if !ok {
		return nil, fmt.Errorf("signalcore: capture has no %q", e.Path)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
