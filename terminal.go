package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"
)

// maxTerminalOutput bounds a terminal's retained output when the agent sets
// no (or a larger) outputByteLimit.
const maxTerminalOutput = 8 << 20

// terminal is one command an agent started through terminal/create. Output
// (stdout and stderr interleaved) keeps the most recent limit bytes, cut at a
// character boundary, as the spec asks.
type terminal struct {
	id  string
	cmd *exec.Cmd

	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
	exit      *acpTerminalExitStatus
	done      chan struct{}
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		start := over
		for start < len(t.buf) && !utf8.RuneStart(t.buf[start]) {
			start++
		}
		t.buf = append(t.buf[:0], t.buf[start:]...)
		t.truncated = true
	}
	return len(p), nil
}

// startTerminal starts the command: an exact argv when args are given, the
// platform shell for a bare command line.
func startTerminal(req acpCreateTerminalRequest, dir string, env []string) (*terminal, error) {
	var cmd *exec.Cmd
	if len(req.Args) > 0 {
		cmd = exec.Command(req.Command, req.Args...)
	} else {
		cmd = shellCommand(req.Command)
	}
	cmd.Dir = dir
	cmd.Env = env
	// A background grandchild holding the output pipe must not keep the
	// terminal "running" after the command itself exited.
	cmd.WaitDelay = 2 * time.Second
	ownProcessGroup(cmd)
	limit := maxTerminalOutput
	if req.OutputByteLimit != nil && *req.OutputByteLimit >= 0 && *req.OutputByteLimit < int64(limit) {
		limit = int(*req.OutputByteLimit)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	t := &terminal{id: "term-" + hex.EncodeToString(b[:]), cmd: cmd, limit: limit, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = t, t
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		st := &acpTerminalExitStatus{}
		if ps := cmd.ProcessState; ps != nil {
			if code := ps.ExitCode(); code >= 0 {
				st.ExitCode = &code
			} else {
				sig := ps.String()
				st.Signal = &sig
			}
		}
		t.exit = st
		t.mu.Unlock()
		close(t.done)
	}()
	return t, nil
}

func (t *terminal) output() acpTerminalOutputResponse {
	t.mu.Lock()
	defer t.mu.Unlock()
	return acpTerminalOutputResponse{Output: string(t.buf), Truncated: t.truncated, ExitStatus: t.exit}
}

func (t *terminal) wait(ctx context.Context) (*acpTerminalExitStatus, error) {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.exit, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// kill stops the command's process tree (SIGTERM, then SIGKILL after a
// short grace) and does not wait longer than that.
func (t *terminal) kill() {
	select {
	case <-t.done:
		return
	default:
	}
	killTree(t.cmd, false)
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		killTree(t.cmd, true)
	}
}
