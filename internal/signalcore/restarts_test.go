package signalcore

import (
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

func TestRecorderRestartsMatchWindowAndCollector(t *testing.T) {
	r := NewRecorder(RecorderConfig{})
	at := time.Now().UTC()
	r.noteRestart("env", "dead", []StreamDef{{Name: "android.frida@1"}, {Name: "test.other@1"}}, at)
	for _, tc := range []struct {
		name     string
		sel      envspec.Selector
		from, to time.Time
		want     int
	}{
		{"failed collector even without events", envspec.Selector{Collectors: []string{"dead"}}, at, at, 1},
		{"fresh instance", envspec.Selector{Collectors: []string{"fresh"}}, at, at, 0},
		{"later window", envspec.Selector{}, at.Add(time.Second), time.Time{}, 0},
		{"earlier window", envspec.Selector{}, time.Time{}, at.Add(-time.Second), 0},
		{"different source", envspec.Selector{Sources: []string{"other"}}, at, at, 0},
		{"different stream", envspec.Selector{Streams: []string{"android.state@1"}}, at, at, 0},
		{"matching stream", envspec.Selector{Streams: []string{"android.frida@1"}}, at, at, 1},
		{"multiple matching streams counted once", envspec.Selector{Streams: []string{"*@1"}}, at, at, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, q, err := r.Query(tc.sel, tc.from, tc.to)
			if err != nil || q.Restarts != tc.want {
				t.Fatalf("restarts = %d, want %d (err %v)", q.Restarts, tc.want, err)
			}
		})
	}
}
