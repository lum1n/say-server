// Command signal-cli-jvm starts signal-cli's JVM build in the Docker image,
// which has no shell for signal-cli's own start script. The image installs
// it as signal-cli where signal-cli has no native build (arm64).
package main

import (
	"fmt"
	"os"
	"syscall"
)

const (
	java = "/opt/java/bin/java"
	// lib holds signal-cli's jars and libsignal_jni.so for the image's
	// architecture.
	lib = "/opt/signal-cli/lib"
)

func main() {
	args := append([]string{java,
		"--enable-native-access=ALL-UNNAMED",
		"-Djava.library.path=" + lib,
		"-cp", lib + "/*",
		"org.asamk.signal.Main",
	}, os.Args[1:]...)
	err := syscall.Exec(java, args, os.Environ())
	fmt.Fprintln(os.Stderr, "signal-cli:", err)
	os.Exit(1)
}
