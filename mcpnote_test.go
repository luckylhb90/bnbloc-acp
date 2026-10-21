package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
)

// unattachableProfile asks for an installed stdio server (mcp-tools), one
// whose command is not installed and an http one.
func unattachableProfile() map[string]any {
	return map[string]any{"mcpServers": []map[string]any{
		{"name": "tools", "command": "mcp-tools"},
		{"name": "browser", "command": "/nonexistent/bnbloc/mcp-browser"},
		{"type": "http", "name": "room", "url": "http://127.0.0.1:9/mcp"},
	}}
}

// A profile server the agent cannot be given is not passed, and the next
// prompt tells the agent which tools it lacks and why, once.
func TestUnattachedMCPServersAreSaidToTheAgent(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "blocks", unattachableProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	got := h.lastText(ws, sid)
	for _, want := range []string{
		"[bnbloc-acp] Tools requested for this session could not be attached",
		"browser (" + dropNoCommand + ")",
		"room (this agent takes no http MCP servers)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("first prompt = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "tools (") || !strings.HasSuffix(got, " | blocks") {
		t.Fatalf("first prompt = %q: the note names only missing servers and precedes the message", got)
	}

	h.send(ws, sid, "profile", core.SessionOptions{})
	h.settle(ws, sid)
	mcp := h.lastText(ws, sid)
	if !strings.Contains(mcp, `"name":"tools","command":"mcp-tools"`) || strings.Contains(mcp, "browser") || strings.Contains(mcp, `"room"`) {
		t.Fatalf("mcpServers = %s (only the installed stdio server)", mcp)
	}
	h.send(ws, sid, "blocks", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); got != "blocks" {
		t.Fatalf("later prompt = %q, want no repeated note", got)
	}
}

// The room sees the missing tools as a step of the turn.
func TestUnattachedMCPServersAreAStepOfTheTurn(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "slow", unattachableProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.waitStep(ws, sid, "tool:Tools not attached: browser, room")
	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
}

// An agent whose config turns profile MCP off is told that too.
func TestMCPOffIsSaidToTheAgent(t *testing.T) {
	off := false
	agent := fakeAgentConfig(t, "fake", nil)
	agent.Profile = &ProfileConfig{MCP: &off}
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "blocks", unattachableProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "tools ("+dropMCPOff+")") || !strings.Contains(got, "room ("+dropMCPOff+")") {
		t.Fatalf("prompt = %q", got)
	}
}

// A session whose servers are all attached gets no note.
func TestAttachedMCPServersNeedNoNote(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "blocks", map[string]any{"mcpServers": []map[string]any{{"name": "tools", "command": "mcp-tools"}}})
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); got != "blocks" {
		t.Fatalf("prompt = %q", got)
	}
}

// commandFound resolves a command the way the agent would start it.
func TestCommandFound(t *testing.T) {
	dir := t.TempDir()
	name := "tool"
	if runtime.GOOS == "windows" {
		name = "tool.exe"
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathEnv := []string{"PATH=/nowhere", "PATH=" + bin}
	cases := []struct {
		command   string
		env       []string
		serverEnv []core.NameValue
		want      bool
	}{
		{filepath.Join(bin, name), nil, nil, true},
		{filepath.Join(bin, "missing"), pathEnv, nil, false},
		{filepath.Join("bin", name), nil, nil, true}, // relative to the workspace
		{"tool", pathEnv, nil, true},                 // the last PATH wins
		{"tool", []string{"PATH=" + bin, "PATH=/nowhere"}, nil, false},
		{"tool", []string{"PATH=/nowhere"}, []core.NameValue{{Name: "PATH", Value: bin}}, true},
		{"missing", pathEnv, nil, false},
		{"", pathEnv, nil, false},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			command   string
			env       []string
			serverEnv []core.NameValue
			want      bool
		}{filepath.Join(bin, "data.txt"), nil, nil, false}) // not executable
	}
	for _, c := range cases {
		if got := commandFound(c.command, dir, c.env, c.serverEnv); got != c.want {
			t.Errorf("commandFound(%q, env %v) = %v, want %v", c.command, c.env, got, c.want)
		}
	}
}

// waitStep polls the running turn until its activity holds step
// ("kind:title").
func (h *harness) waitStep(ws, sid, step string) turnStatus {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ts := h.pollNow(ws, sid)
		for _, a := range ts.Activity {
			if a.Kind+":"+a.Title == step {
				return ts
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no step %q: %+v", step, ts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
