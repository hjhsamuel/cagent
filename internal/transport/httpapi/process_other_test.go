//go:build !windows

package httpapi_test

import "os/exec"

func hideTestProcess(cmd *exec.Cmd) {}
