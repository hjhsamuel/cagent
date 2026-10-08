//go:build !windows

package mongodb

import "os/exec"

func hideTestProcess(cmd *exec.Cmd) {}
