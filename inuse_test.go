package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
)

func readInUse(t *testing.T, home string) inUseDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, inUseFile))
	if err != nil {
		t.Fatal(err)
	}
	var d inUseDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A copy of the test binary (the fake agent) at path: a "version" of the agent.
func copyAgentBinary(t *testing.T, path string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInUseFileSaysWhatLiveSessionsRunFrom(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", map[string]string{"WHERE_IS_THE_AGENT": "/store/pi/1.4.0/bin/pi", "PLAIN": "value"})
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)

	d := readInUse(t, h.home)
	if d.Stopped || d.PID != os.Getpid() || time.Since(time.UnixMilli(d.UpdatedAt)) > time.Minute || len(d.Sessions) != 1 {
		t.Fatalf("a live session is listed by a live core: %+v", d)
	}
	refs := strings.Join(d.Sessions[0].Refs, "|")
	for _, want := range []string{agent.Command, "/store/pi/1.4.0/bin/pi"} {
		if !strings.Contains(refs, want) {
			t.Errorf("the command and a path in the environment are held (%q): %s", want, refs)
		}
	}
	if strings.Contains(refs, "value") {
		t.Errorf("a value that is not a path is not listed: %s", refs)
	}

	if err := h.client.DeleteWorkspace(h.ctx(), h.ep, ws); err != nil {
		t.Fatal(err)
	}
	if d = readInUse(t, h.home); len(d.Sessions) != 0 {
		t.Errorf("a deleted workspace's sessions are released: %+v", d)
	}
	h.a.close()
	if d = readInUse(t, h.home); !d.Stopped {
		t.Errorf("a core that exits says so: %+v", d)
	}
}

// The conversation is kept when the program the session was created with is gone
// (an update that removed it): reproduced against the real core and a real model,
// where the third version's cleanup removed the file and the session was lost.
func TestSessionKeepsItsConversationWhenItsProgramIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("copies and removes a running binary's file")
	}
	store := t.TempDir()
	v1 := filepath.Join(store, "1.0.0", "agent")
	v2 := filepath.Join(store, "2.0.0", "agent")
	copyAgentBinary(t, v1)
	copyAgentBinary(t, v2)
	at := func(cmd string) AgentConfig {
		a := fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_LOAD": "1"})
		a.Command = cmd
		return a
	}
	home := t.TempDir()
	h := newHarnessAt(t, Config{Agents: []AgentConfig{at(v1)}}, home)
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	h.a.mu.Lock()
	s := h.a.sessions[sid]
	h.a.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	h.a.reapOnce(time.Millisecond) // idle stop

	// The agent is registered at 2.0.0 now and 1.0.0 is gone, as after two updates.
	raw, _ := json.Marshal(Config{Agents: []AgentConfig{at(v2)}})
	if err := os.WriteFile(filepath.Join(home, "agents.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(v1)); err != nil {
		t.Fatal(err)
	}
	h.send(ws, sid, "whoami", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("the conversation was lost with its program: %+v", ts)
	}
	s.mu.Lock()
	cmd := s.agent.Command
	s.mu.Unlock()
	if cmd != v2 {
		t.Errorf("the session continues with the agent's current launch: %q, want %q", cmd, v2)
	}
	if got := strings.Join(texts(h.messages(ws, sid, 0)), "|"); !strings.Contains(got, "resumed=true") {
		t.Errorf("it continued the same ACP session: %s", got)
	}
}

// A session whose files all exist is never touched: it continues with the launch
// it was created with, whatever the agent is registered as now.
func TestSessionWithItsProgramPresentKeepsItsOwnLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("copies a binary")
	}
	store := t.TempDir()
	v1 := filepath.Join(store, "1.0.0", "agent")
	v2 := filepath.Join(store, "2.0.0", "agent")
	copyAgentBinary(t, v1)
	copyAgentBinary(t, v2)
	at := func(cmd string) AgentConfig {
		a := fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_LOAD": "1"})
		a.Command = cmd
		return a
	}
	home := t.TempDir()
	h := newHarnessAt(t, Config{Agents: []AgentConfig{at(v1)}}, home)
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	h.a.mu.Lock()
	s := h.a.sessions[sid]
	h.a.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	h.a.reapOnce(time.Millisecond)
	raw, _ := json.Marshal(Config{Agents: []AgentConfig{at(v2)}})
	if err := os.WriteFile(filepath.Join(home, "agents.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.send(ws, sid, "whoami", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("%+v", ts)
	}
	s.mu.Lock()
	cmd := s.agent.Command
	s.mu.Unlock()
	if cmd != v1 {
		t.Errorf("nothing was missing, so the launch is the one the session was created with: %q, want %q", cmd, v1)
	}
}
