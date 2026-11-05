package envcap

import "strings"

// androidExecTools are the Android shell tools whose process exit
// code does not reflect success: am, pm and cmd report failures on
// their output while still exiting 0.
var androidExecTools = map[string]bool{"am": true, "pm": true, "cmd": true}

// androidExecOutcome classifies an Android shell command for
// env.exec. ok is false when the command runs one of
// androidExecTools and its output carries a failure signature, with
// reason the first such line; for every other command ok mirrors
// exit == 0. stderr is checked first (am writes its errors there).
func androidExecOutcome(argv []string, exit int, stdout, stderr []byte) (reason string, ok bool) {
	if !androidExecTools[execTool(argv)] {
		return "", exit == 0
	}
	if line := firstFailureLine(stderr); line != "" {
		return line, false
	}
	if line := firstFailureLine(stdout); line != "" {
		return line, false
	}
	return "", exit == 0
}

// execTool is the base name of the tool a command runs, seeing
// through a `sh -c "<line>"` wrapper to the first word of the line.
func execTool(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	tool := argv[0]
	if (tool == "sh" || tool == "bash" || tool == "/system/bin/sh") && len(argv) >= 3 && argv[1] == "-c" {
		if fields := strings.Fields(argv[2]); len(fields) > 0 {
			tool = fields[0]
		}
	}
	if i := strings.LastIndexByte(tool, '/'); i >= 0 {
		tool = tool[i+1:]
	}
	return tool
}

// firstFailureLine returns the first output line that signals an
// Android tool failure, or "".
func firstFailureLine(b []byte) string {
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "Error") ||
			strings.HasPrefix(line, "Exception") ||
			strings.HasPrefix(line, "Failure") ||
			strings.Contains(line, "Activity not started") {
			return line
		}
	}
	return ""
}
