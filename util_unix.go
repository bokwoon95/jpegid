//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func stop(cmd *exec.Cmd) {
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
}

func setpgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
