package androidemu

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
)

// sdkmanager and avdmanager are Java programs: without a Java to run them,
// installing an image and creating an emulator fail with "exit status 1"
// and nothing else said. A machine with the Android SDK from Android
// Studio often has no Java on its PATH and no JAVA_HOME, but Studio ships
// one (its JetBrains Runtime), so that one is used when there is no other.

// lookPath finds a program on the PATH; tests replace it.
var lookPath = osexec.LookPath

// studioRuntimes are where Android Studio keeps its bundled Java, by OS.
func studioRuntimes() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		return []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Android", "Android Studio", "jbr"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Android Studio", "jbr"),
		}
	case "darwin":
		return []string{
			"/Applications/Android Studio.app/Contents/jbr/Contents/Home",
			filepath.Join(home, "Applications", "Android Studio.app", "Contents", "jbr", "Contents", "Home"),
		}
	default:
		return []string{"/opt/android-studio/jbr", filepath.Join(home, "android-studio", "jbr")}
	}
}

// hasJava says whether dir is a Java home: it holds bin/java.
func hasJava(dir string) bool {
	if dir == "" {
		return false
	}
	name := "java"
	if runtime.GOOS == "windows" {
		name = "java.exe"
	}
	info, err := os.Stat(filepath.Join(dir, "bin", name))
	return err == nil && !info.IsDir()
}

// javaHome is the Java the SDK's tools are run with: JAVA_HOME when it
// holds one, then none to set when java is on the PATH, then Android
// Studio's bundled runtime. found is false when there is no Java at all.
func javaHome(candidates []string) (home string, found bool) {
	if set := os.Getenv("JAVA_HOME"); hasJava(set) {
		return set, true
	}
	if _, err := lookPath("java"); err == nil {
		return "", true
	}
	for _, dir := range candidates {
		if hasJava(dir) {
			return dir, true
		}
	}
	return "", false
}
