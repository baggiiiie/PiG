//go:build windows

package main

import (
	"fmt"
	"os"
)

// Windows implements Node child.kill() as process termination.
func terminatePackageCapture(process *os.Process) {
	_ = process.Kill()
}

func packageCaptureExitStatus(state *os.ProcessState) string {
	return fmt.Sprintf("code %d", state.ExitCode())
}
