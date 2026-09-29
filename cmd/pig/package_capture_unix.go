//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Node child.kill() defaults to SIGTERM and leaves settlement to the close event.
func terminatePackageCapture(process *os.Process) {
	_ = process.Signal(syscall.SIGTERM)
}

func packageCaptureExitStatus(state *os.ProcessState) string {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return "signal " + unix.SignalName(status.Signal())
	}
	return fmt.Sprintf("code %d", state.ExitCode())
}
