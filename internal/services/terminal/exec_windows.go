//go:build windows

package terminal

import (
	"os/exec"
	"syscall"
)

// hideWindow hides the console window on Windows for the given command.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
	}
}
