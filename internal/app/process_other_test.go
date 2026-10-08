//go:build !windows

package app

import "os/exec"

func hideTestProcess(cmd *exec.Cmd) {}
