//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// ownProcessGroup is a no-op on Windows: killTree ends the tree by pid.
func ownProcessGroup(cmd *exec.Cmd) {}

// killTree ends cmd and its children (taskkill /T); Windows has no graceful
// signal for a console child, so hard and soft are the same.
func killTree(cmd *exec.Cmd, hard bool) {
	if cmd.Process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

// shellCommand runs a command line through the platform shell.
func shellCommand(line string) *exec.Cmd {
	return exec.Command("cmd", "/C", line)
}
