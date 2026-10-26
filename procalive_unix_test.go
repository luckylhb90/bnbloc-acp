//go:build !windows

package main

import "syscall"

// processAlive reports whether pid is a live process (signal 0).
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
