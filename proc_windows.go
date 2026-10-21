//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// executable reports whether path names an existing file, trying the
// PATHEXT extensions for a name without one (as Windows resolves commands).
func executable(path string) bool {
	if filepath.Ext(path) != "" {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular()
	}
	exts := os.Getenv("PATHEXT")
	if exts == "" {
		exts = ".COM;.EXE;.BAT;.CMD"
	}
	for _, ext := range strings.Split(exts, ";") {
		if ext == "" {
			continue
		}
		if st, err := os.Stat(path + strings.ToLower(ext)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// envKeyEqual compares environment variable names (case-insensitive here).
func envKeyEqual(a, b string) bool { return strings.EqualFold(a, b) }
