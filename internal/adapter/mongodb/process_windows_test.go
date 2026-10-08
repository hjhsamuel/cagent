//go:build windows

package mongodb

import (
	"os/exec"
	"syscall"
)

func hideTestProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
