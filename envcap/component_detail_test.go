package envcap

import "testing"

// A component that works says its last failure is past; one that doesn't
// says it as it is.
func TestAComponentsDetailSaysWhetherItsFailureIsPast(t *testing.T) {
	for _, c := range []struct{ state, failure, want string }{
		{"ok", "", ""},
		{"ok", "frida-server not running", "repaired; it last failed: frida-server not running"},
		{"failed", "frida-server not running", "frida-server not running"},
		{"degraded", "x", "x"},
	} {
		if got := componentDetail(c.state, c.failure); got != c.want {
			t.Errorf("%s %q: got %q, want %q", c.state, c.failure, got, c.want)
		}
	}
}
