//go:build windows

package configvalue

import (
	"context"
	"os/exec"
	"strings"
	"syscall"

	"github.com/MichaelKinsy/PiG/internal/crossspawn"
)

// runShellCommand mirrors executeCommandUncached on win32: use the configured bash before falling back to the default shell if bash could not start.
func runShellCommand(ctx context.Context, payload string) (string, bool) {
	return runShellCommandWithConfiguredShell(ctx, payload)
}

func configuredShellCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// runDefaultShell mirrors upstream executeWithDefaultShell: Node's execSync
// runs %ComSpec% /d /s /c "<command>" for cmd, or -c for a non-cmd ComSpec.
func runDefaultShell(ctx context.Context, payload string) (string, bool) {
	cmd := crossspawn.ShellCommand(ctx, payload)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(out))
	return value, value != ""
}
