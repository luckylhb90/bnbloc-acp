//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in its own process group so the whole tree (an
// npx wrapper and the node process it starts, a shell and its children) can
// be stopped together.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree signals cmd's process group: SIGTERM first (hard=false), SIGKILL
// when hard.
func killTree(cmd *exec.Cmd, hard bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// shellCommand runs a command line through the platform shell.
func shellCommand(line string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", line)
}
