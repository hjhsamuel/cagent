//go:build !windows

package local

import "os/exec"

func configureProcess(*exec.Cmd) {}
