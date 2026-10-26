package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
)

// skipWithoutSh skips a test whose provider is a #!/bin/sh script: Windows cannot
// run one (the provider's own code is covered by the other platforms).
func skipWithoutSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test provider is a shell script")
	}
}

// fakeProvider is an environment provider script. It records each start (its pid
// on a line of "started") and its end ("stopped", written when its stdin
// closes), prints the given lines and READY, then waits for stdin to close, as a
// real one does. after, when set, is run instead of waiting (a provider that
// ends early).
func fakeProvider(t *testing.T, lines []string, after string) (cfg *EnvProviderConfig, started, stopped string) {
	t.Helper()
	skipWithoutSh(t)
	dir := t.TempDir()
	started, stopped = filepath.Join(dir, "started"), filepath.Join(dir, "stopped")
	var b strings.Builder
	b.WriteString("#!/bin/sh\necho $$ >> '" + started + "'\n")
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + strings.ReplaceAll(l, "'", `'\''`) + "'\n")
	}
	b.WriteString("echo READY\n")
	if after != "" {
		b.WriteString(after + "\n")
	} else {
		b.WriteString("cat >/dev/null\necho stopped > '" + stopped + "'\n")
	}
	path := filepath.Join(dir, "provider")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return &EnvProviderConfig{Command: path}, started, stopped
}

func waitFile(t *testing.T, path string, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

// running counts the providers of a "started" file that are still alive (the
// catalog probe starts one too, and ends it).
func running(t *testing.T, started string) int {
	t.Helper()
	raw, _ := os.ReadFile(started)
	n := 0
	for _, f := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(f)
		if err == nil && processAlive(pid) {
			n++
		}
	}
	return n
}

func providerHarness(t *testing.T, cfg *EnvProviderConfig, env ...map[string]string) *harness {
	t.Helper()
	var e map[string]string
	if len(env) > 0 {
		e = env[0]
	}
	a := fakeAgentConfig(t, "fake", e)
	a.EnvProvider = cfg
	return newHarness(t, Config{Agents: []AgentConfig{a}})
}

// What the provider prints is in the agent's environment, and the provider ends
// when the agent's session does (its stdin closes).
func TestEnvProviderVariablesReachTheAgentAndItEndsWithIt(t *testing.T) {
	cfg, started, stopped := fakeProvider(t, []string{"PROVIDED_ENDPOINT=http://127.0.0.1:5555/v1", "PROVIDED_JSON={\"a\":[1,2],\"b\":\"x y\"}"}, "")
	h := providerHarness(t, cfg)
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "getenv PROVIDED_ENDPOINT", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); got != "http://127.0.0.1:5555/v1" {
		t.Fatalf("PROVIDED_ENDPOINT in the agent = %q", got)
	}
	h.send(ws, sid, "getenv PROVIDED_JSON", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); got != `{"a":[1,2],"b":"x y"}` {
		t.Fatalf("a value with quotes and spaces came through as %q", got)
	}
	if n := running(t, started); n != 1 {
		t.Fatalf("%d providers run for one agent process", n)
	}
	if err := h.client.DeleteWorkspace(h.ctx(), h.ep, ws); err != nil {
		t.Fatal(err)
	}
	waitFile(t, stopped, "the provider's end after its agent's")
	if n := running(t, started); n != 0 {
		t.Fatalf("%d providers outlive their agents", n)
	}
}

// A provider that fails, prints what is not a variable, or names a variable the
// core keeps from agents refuses the session with the reason, and its stderr
// (local diagnostics) never reaches the wire.
func TestEnvProviderFailuresRefuseTheSession(t *testing.T) {
	skipWithoutSh(t)
	script := func(body string) *EnvProviderConfig {
		p := filepath.Join(t.TempDir(), "p")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return &EnvProviderConfig{Command: p}
	}
	for name, c := range map[string]struct {
		cfg  *EnvProviderConfig
		want string
	}{
		"exits before READY":    {script("echo SECRET-DETAIL >&2; exit 3"), "exited before it printed READY"},
		"not a variable":        {script("echo 'this is not a variable'; echo READY; cat >/dev/null"), "not NAME=VALUE"},
		"a name the core keeps": {script("echo BNBLOC_ACP_TOKEN=x; echo READY; cat >/dev/null"), "keeps from agents"},
		"a bad name":            {script("echo 1BAD=x; echo READY; cat >/dev/null"), "not NAME=VALUE"},
		"not installed":         {&EnvProviderConfig{Command: "/no/such/provider"}, "not found"},
	} {
		h := providerHarness(t, c.cfg)
		ws, _ := h.workspace()
		_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "whoami"})
		if err == nil {
			t.Errorf("%s: the session was created", name)
			continue
		}
		if !strings.Contains(err.Error(), "environment provider") && name != "not installed" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q, want it to name the environment provider and %q", name, err, c.want)
		}
		if strings.Contains(err.Error(), "SECRET-DETAIL") {
			t.Errorf("%s: the provider's stderr reached the wire: %q", name, err)
		}
	}
}

// A sandboxed session starts its provider on the host, not through the confining
// helper (it holds the credential the agent must not), and the agent, confined,
// still gets what it printed. The sandbox descriptor wins over a provider that
// tries to set one of its variables.
func TestEnvProviderRunsOnTheHostForASandboxedSession(t *testing.T) {
	cfg, started, _ := fakeProvider(t, []string{"PROVIDED_ENDPOINT=http://127.0.0.1:5555/v1", "BNBLOC_VMEXEC_SPACE=provider-says"}, "")
	h := sandboxHarness(t, nil)
	h.a.reg.cfg.Agents[0].EnvProvider = cfg
	vmexec, logPath := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "token-1", "space-a")
	sid := h.sandboxSession(ws, exe)
	in := h.readInfo(ws, sid)
	if in.Space != "space-a" {
		t.Fatalf("the sandbox descriptor's space = %q: a provider overrode it", in.Space)
	}
	h.sendSandboxed(ws, sid, "getenv PROVIDED_ENDPOINT", exe)
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); got != "http://127.0.0.1:5555/v1" {
		t.Fatalf("PROVIDED_ENDPOINT in the confined agent = %q", got)
	}
	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), cfg.Command) {
		t.Fatalf("the provider went through the confining helper:\n%s", raw)
	}
	if n := running(t, started); n != 1 {
		t.Fatalf("%d providers run for the sandboxed agent", n)
	}
}

// A provider that ends before its agent ends the agent: its endpoint is gone.
// The next input starts both again and the conversation continues.
func TestEnvProviderEndingFirstEndsTheAgent(t *testing.T) {
	cfg, started, _ := fakeProvider(t, []string{"PROVIDED=1"}, "sleep 1")
	h := providerHarness(t, cfg, map[string]string{"ACP_FAKE_RESUME": "1"})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "whoami", core.SessionOptions{})
	h.settle(ws, sid)
	time.Sleep(3 * time.Second) // the provider is gone; so is the agent
	h.send(ws, sid, "whoami", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "resumed=true") {
		t.Fatalf("the next input did not continue the session in a new process: %q", got)
	}
	if raw, _ := os.ReadFile(started); strings.Count(string(raw), "\n") < 2 {
		t.Fatalf("the provider was not started again: %q", raw)
	}
}

func TestEnvProviderConfigNeedsACommand(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"agents":[{"name":"a","command":"x","envProvider":{"command":"  "}}]}`)); err == nil {
		t.Fatal("an environment provider with no command was accepted")
	}
	if _, err := ParseConfig([]byte(`{"agents":[{"name":"a","command":"x","envProvider":{"command":"p","args":["route","provide"]}}]}`)); err != nil {
		t.Fatal(err)
	}
}
