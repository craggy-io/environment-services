package envcap

import "testing"

// Ported verbatim from the old module's envcap suite: Android's
// exit-code-blind tools must not read as success through env.exec.
func TestAndroidExecOutcome(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		exit       int
		stdout     string
		stderr     string
		wantOK     bool
		wantReason string
	}{
		{
			name:       "am start resolve failure exits zero",
			argv:       []string{"am", "start", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", "com.whatsapp"},
			exit:       0,
			stderr:     "Error: Activity not started, unable to resolve Intent { act=android.intent.action.MAIN }",
			wantOK:     false,
			wantReason: "Error: Activity not started, unable to resolve Intent { act=android.intent.action.MAIN }",
		},
		{
			name:   "am start success",
			argv:   []string{"am", "start", "-n", "com.example/.Main"},
			exit:   0,
			stdout: "Starting: Intent { cmp=com.example/.Main }",
			wantOK: true,
		},
		{
			name:       "pm install failure line",
			argv:       []string{"pm", "install", "/data/local/tmp/app.apk"},
			exit:       0,
			stdout:     "Failure [INSTALL_FAILED_INVALID_APK]",
			wantOK:     false,
			wantReason: "Failure [INSTALL_FAILED_INVALID_APK]",
		},
		{
			name:   "pm install success",
			argv:   []string{"pm", "install", "/data/local/tmp/app.apk"},
			exit:   0,
			stdout: "Success",
			wantOK: true,
		},
		{
			name:       "am through sh -c wrapper",
			argv:       []string{"sh", "-c", "am start -p com.whatsapp"},
			exit:       0,
			stderr:     "Error: Activity not started",
			wantOK:     false,
			wantReason: "Error: Activity not started",
		},
		{
			name:   "non-android tool keeps exit semantics on zero",
			argv:   []string{"ls", "-l"},
			exit:   0,
			stdout: "Error: this is just data, not an am failure",
			wantOK: true,
		},
		{
			name:   "non-android tool reports its own non-zero exit",
			argv:   []string{"ls", "/missing"},
			exit:   2,
			stderr: "ls: /missing: No such file or directory",
			wantOK: false,
		},
		{
			name:   "am with unreliable zero exit and clean output is ok",
			argv:   []string{"am", "force-stop", "com.whatsapp"},
			exit:   0,
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := androidExecOutcome(tc.argv, tc.exit, []byte(tc.stdout), []byte(tc.stderr))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
