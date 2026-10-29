package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
)

// A guest agent exists only in the member's sandbox: its command is a command
// line of the guest, started through `vmexec run --stdio`. The tests use a fake
// vmexec that runs that command line with the host shell (the "guest" here), and
// the test binary as the agent (the command line is `ACP_FAKE_AGENT=1 <binary>`,
// so a guest agent's environment is a command-line matter, as in the guest).

// fakeGuestVMExec records its arguments and runs the command line of
// `run --stdio -- <line>` with sh.
func fakeGuestVMExec(t *testing.T) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	path = filepath.Join(dir, "bnbloc-vmexec")
	script := `#!/bin/sh
{ printf '%s\n' "$@"; echo '---'; } >> "` + log + `"
[ "$1" = run ] && [ "$2" = --stdio ] && [ "$3" = -- ] || exit 64
exec sh -c "$4"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func guestAgent(t *testing.T, name, extraFlags string) AgentConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return AgentConfig{Name: name, Command: "ACP_FAKE_AGENT=1 GORACE=atexit_sleep_ms=0 " + extraFlags + " " + shellQuote(exe), Sandbox: sandboxGuest}
}

func guestHarness(t *testing.T, a AgentConfig, home string) *harness {
	t.Helper()
	needSandboxHere(t)
	return newHarnessAt(t, Config{Agents: []AgentConfig{a}}, home)
}

func TestGuestAgentIsListedWithoutBeingStartedAndRequiresTheSandbox(t *testing.T) {
	needSandboxHere(t)
	// A command that exists nowhere on this computer: if the core looked it up or
	// started it on the host, the agent would be unavailable.
	a := AgentConfig{Name: "ghost", Command: "$HOME/agents/ghost/bin/ghost", Args: []string{"acp"}, Sandbox: sandboxGuest}
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	ts, err := h.client.ListTargets(h.ctx(), h.ep)
	if err != nil || len(ts) != 1 || ts[0].ID != "ghost" || !ts[0].RequiresSandbox {
		t.Fatalf("a guest agent is listed at once and says it requires the sandbox: %+v %v", ts, err)
	}
	if st := h.a.reg.statuses(); len(st) != 1 || !st[0].Available || st[0].Sandbox != sandboxGuest {
		t.Fatalf("status = %+v", st)
	}
}

func TestGuestAgentIsRefusedWithoutASandbox(t *testing.T) {
	h := guestHarness(t, guestAgent(t, "fake", ""), t.TempDir())
	ws, _ := h.workspace()
	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hello"})
	if err == nil || !strings.Contains(err.Error(), "exists only in the agent sandbox") {
		t.Fatalf("a request without a sandbox must be refused, never run on the host: %v", err)
	}
	var pe interface{ Error() string }
	if !errors.As(err, &pe) {
		t.Fatal(err)
	}
}

func TestGuestAgentStartsThroughVMExecRunStdioWithTheDescriptor(t *testing.T) {
	h := guestHarness(t, guestAgent(t, "fake", ""), t.TempDir())
	vmexec, logPath := fakeGuestVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "token-g", "space-g")
	sid := h.sandboxSession(ws, exe)
	in := h.readInfo(ws, sid)

	raw, _ := os.ReadFile(logPath)
	args := strings.Split(strings.TrimSpace(strings.Split(string(raw), "---")[0]), "\n")
	if len(args) != 4 || args[0] != "run" || args[1] != "--stdio" || args[2] != "--" || !strings.Contains(args[3], "ACP_FAKE_AGENT=1") {
		t.Fatalf("vmexec was called as %v; want run --stdio -- <the command line>", args)
	}
	if strings.Contains(string(raw), "--harness") || strings.Contains(string(raw), "confine") {
		t.Fatalf("nothing of a guest agent runs on the host, so nothing is confined: %s", raw)
	}
	if in.Space != "space-g" || in.Token != "token-g" || in.TokenFile == "" || in.TokenEnv != "" {
		t.Fatalf("descriptor: space=%q token=%q file=%q env=%q", in.Space, in.Token, in.TokenFile, in.TokenEnv)
	}
	if in.Terminal {
		t.Fatal("the client terminals were offered to a guest agent: their commands would run on the host")
	}
	// A turn works end to end.
	h.sendSandboxed(ws, sid, "hello again", exe)
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("turn failed: %+v", ts)
	}
}

func TestGuestAgentArgumentsAreQuotedForTheGuestShell(t *testing.T) {
	l := &sandboxLaunch{vmexec: "/v", guest: true, dir: "/w"}
	cmd := l.command("$HOME/bin/agent", []string{"acp", "--name", "a b", "it's", "", "x;y"})
	got := cmd.Args[len(cmd.Args)-1]
	want := `$HOME/bin/agent acp --name 'a b' 'it'\''s' '' 'x;y'`
	if got != want {
		t.Fatalf("command line = %q, want %q", got, want)
	}
	if cmd.Args[1] != "run" || cmd.Args[2] != "--stdio" || cmd.Args[3] != "--" {
		t.Fatalf("argv = %v", cmd.Args)
	}
}

func TestGuestAgentContinuesAfterTheIdleStop(t *testing.T) {
	h := guestHarness(t, guestAgent(t, "fake", "ACP_FAKE_LOAD=1"), t.TempDir())
	vmexec, logPath := fakeGuestVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "t1", "space-g")
	sid := h.sandboxSession(ws, exe)
	h.settle(ws, sid)
	h.a.mu.Lock()
	s := h.a.sessions[sid]
	h.a.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	h.a.reapOnce(time.Millisecond)
	s.mu.Lock()
	stopped := s.proc == nil
	s.mu.Unlock()
	if !stopped {
		t.Fatal("the idle guest agent was not stopped")
	}
	h.sendSandboxed(ws, sid, "whoami", sbExecution(dir, vmexec, "t2", "space-g"))
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("the continued turn failed: %+v", ts)
	}
	if got := strings.Join(texts(h.messages(ws, sid, 0)), "|"); !strings.Contains(got, "resumed=true") {
		t.Fatalf("it must continue the same ACP session: %s", got)
	}
	raw, _ := os.ReadFile(logPath)
	if n := strings.Count(string(raw), "---"); n != 2 {
		t.Fatalf("vmexec was started %d times, want 2 (the session, then its continuation)", n)
	}
}

// A guest agent's files are in the guest: the "program is gone" fallback of a
// host session must never look for them on the host (it would find them missing
// and swap the launch).
func TestGuestAgentIsNeverRelaunchedFromTheHostsPointOfView(t *testing.T) {
	home := t.TempDir()
	a := guestAgent(t, "fake", "")
	a.Command = "/guest/only/path/agent"
	h := newHarnessAt(t, Config{Agents: []AgentConfig{a}}, home)
	s := &session{agent: a, reg: h.a.reg}
	s.relaunchFromCurrent()
	if s.agent.Command != "/guest/only/path/agent" {
		t.Fatalf("the launch was swapped: %q", s.agent.Command)
	}
}

func TestGuestAgentConfigRefusesWhatItCannotDeliver(t *testing.T) {
	for name, body := range map[string]string{
		"env":         `{"agents":[{"name":"g","command":"x","sandbox":"guest","env":{"K":"v"}}]}`,
		"envProvider": `{"agents":[{"name":"g","command":"x","sandbox":"guest","envProvider":{"command":"p"}}]}`,
	} {
		if _, err := ParseConfig([]byte(body)); err == nil || !strings.Contains(err.Error(), "guest") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseConfig([]byte(`{"agents":[{"name":"g","command":"$HOME/a/b acp","sandbox":"guest"}]}`)); err != nil {
		t.Errorf("a plain guest agent: %v", err)
	}
	// models and modelArgs work as for a host agent: the chosen model is added to the
	// command line the guest runs.
	c, err := ParseConfig([]byte(`{"agents":[{"name":"g","command":"$HOME/a/b","args":["--acp"],"sandbox":"guest","models":["m1","m2"],"modelArgs":["--model","{model}"]}]}`))
	if err != nil {
		t.Fatalf("a guest agent with modelArgs: %v", err)
	}
	if got := strings.Join(c.Agents[0].spawnArgs("m2"), " "); got != "--acp --model m2" {
		t.Errorf("a guest agent's launch args = %q", got)
	}
}
