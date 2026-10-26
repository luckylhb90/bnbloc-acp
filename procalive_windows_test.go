//go:build windows

package main

import "os"

// processAlive reports whether pid is a live process: Windows opens it, and
// a process that ended cannot be opened for a signal.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer p.Release()
	return true
}
