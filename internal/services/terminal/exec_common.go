//go:build !windows

package terminal

import (
	"os/exec"
)

// hideWindow hides the console window on Windows for the given command.
// This is a no-op on non-Windows platforms.
func hideWindow(cmd *exec.Cmd) {
	_ = cmd
}
