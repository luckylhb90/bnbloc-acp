package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// Sandboxed sessions are tested with a fake vmexec: a script that records how
// it was called and then runs the agent confine would have run. The agent is
// the test binary (fakeagent_test.go), which reports what it was started with.

func needSandboxHere(t *testing.T) {
	t.Helper()
	if !sandboxHere() {
		t.Skip("the agent sandbox confines on macOS and Linux only")
	}
}

// fakeVMExec writes the script and returns its path and the log it appends to
// (one block per call: its arguments, one per line, then "---").
func fakeVMExec(t *testing.T) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	path = filepath.Join(dir, "bnbloc-vmexec")
	script := `#!/bin/sh
{ printf '%s\n' "$@"; echo '---'; } >> "` + log + `"
[ "$1" = confine ] || exit 64
shift
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

// sandboxHarness is a harness whose agent can run sandboxed (routing opencode,
// its warm-up stubbed: the real one runs the installed opencode).
func sandboxHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	return sandboxHarnessAt(t, env, t.TempDir())
}

func sandboxHarnessAt(t *testing.T, env map[string]string, home string) *harness {
	t.Helper()
	needSandboxHere(t)
	old := warmOpencodeFn
	warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error { return nil }
	t.Cleanup(func() { warmOpencodeFn = old })
	a := fakeAgentConfig(t, "fake", env)
	a.Sandbox = sandboxRoutingOpencode
	return newHarnessAt(t, Config{Agents: []AgentConfig{a}}, home)
}

func (h *harness) workdirWorkspace(dir string) string {
	h.t.Helper()
	id, err := h.client.CreateWorkspaceAt(h.ctx(), h.ep, dir)
	if err != nil {
		h.t.Fatalf("CreateWorkspaceAt: %v", err)
	}
	return id
}

func sbExecution(workdir, vmexec, token, space string) *core.Execution {
	return &core.Execution{Sandbox: core.SandboxExecution{
		URL: "http://127.0.0.1:7311", ExecToken: token, ExpiresAtMs: 42, Machine: "m", Space: space, VMExec: vmexec, Workdir: workdir,
		Mounts:      []core.PathMount{{Host: workdir, Guest: workdir}},
		Passthrough: []string{"gh"},
		HostConfine: core.HostConfine{Workspace: workdir, DenyWrite: []string{workdir + "/.room/"}, DenyRead: []string{"/h/.config/acme/mail"}},
	}}
}

type procInfo struct {
	PID        int    `json:"pid"`
	Cwd        string `json:"cwd"`
	Pwd        string `json:"pwd"`
	Terminal   bool   `json:"terminal"`
	TokenEnv   string `json:"tokenEnv"`
	TokenFile  string `json:"tokenFile"`
	Token      string `json:"token"`
	Config     string `json:"config"`
	Space      string `json:"space"`
	DenyRead   string `json:"denyRead"`
	ACPEnvLeak bool   `json:"acpEnvLeak"`
	// ClaudePrefix and NPMCache are the claude routing's prefix and the
	// confined agent's own npm cache.
	ClaudePrefix string `json:"claudePrefix"`
	// ExecServer is codex's remote executor, the bridge into the space.
	ExecServer string `json:"execServer"`
	NPMCache   string `json:"npmCache"`
}

func (h *harness) info(ws, sid string) procInfo {
	h.t.Helper()
	h.send(ws, sid, "sandboxinfo", core.SessionOptions{})
	return h.readInfo(ws, sid)
}

func (h *harness) readInfo(ws, sid string) procInfo {
	h.t.Helper()
	h.settle(ws, sid)
	var in procInfo
	if err := json.Unmarshal([]byte(h.lastText(ws, sid)), &in); err != nil {
		h.t.Fatalf("sandboxinfo = %q: %v", h.lastText(ws, sid), err)
	}
	return in
}

func (h *harness) sandboxSession(ws string, exe *core.Execution) string {
	h.t.Helper()
	return h.session(ws, "fake", "sandboxinfo", core.SessionOptions{Execution: exe})
}

func (h *harness) sendSandboxed(ws, sid, text string, exe *core.Execution) {
	h.t.Helper()
	h.send(ws, sid, text, core.SessionOptions{Execution: exe})
}

func TestSandboxConfigValidation(t *testing.T) {
	for _, c := range []struct {
		sandbox string
		ok      bool
	}{{"", true}, {"opencode", true}, {"claude", true}, {"off", true}, {"codex", true}, {"OpenCode", false}} {
		_, err := ParseConfig([]byte(`{"agents":[{"name":"a","command":"x","sandbox":"` + c.sandbox + `"}]}`))
		if (err == nil) != c.ok {
			t.Errorf("sandbox %q: err = %v, want ok=%v", c.sandbox, err, c.ok)
		}
	}
}

func TestSandboxExecIsAdvertisedWhereTheHelperCanConfine(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	want := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	if got := core.Supports(h.ctx(), h.client, h.ep, core.CapabilitySandboxExec); got != want {
		t.Fatalf("sandboxExec advertised = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

// A workdir workspace adopts a directory the core does not own: deleting it
// forgets the workspace and leaves the directory and its files alone.
func TestWorkdirWorkspaceIsAdoptedNeverOwned(t *testing.T) {
	h := sandboxHarness(t, nil)
	dir := t.TempDir()
	keep := filepath.Join(dir, "work.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := h.workdirWorkspace(dir)
	got, err := h.a.ws.get(ws)
	if err != nil || !got.workdirBacked || !sameDir(got.real, dir) {
		t.Fatalf("workspace = %+v, %v", got, err)
	}
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, ".room/INDEX.md", "records"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".room", "INDEX.md")); err != nil {
		t.Fatalf("the room context did not land in the workdir: %v", err)
	}
	if err := h.client.DeleteWorkspace(h.ctx(), h.ep, ws); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(keep); err != nil || string(raw) != "mine" {
		t.Fatalf("deleting the workspace touched the directory: %q %v", raw, err)
	}
	if _, err := h.a.ws.get(ws); err == nil {
		t.Fatal("the workspace is still known after delete")
	}
	if _, err := os.Stat(filepath.Join(h.home, "workspaces", ws+".workdir")); !os.IsNotExist(err) {
		t.Fatalf("the record is still there: %v", err)
	}
}

func TestWorkdirWorkspaceRefusals(t *testing.T) {
	h := sandboxHarness(t, nil)
	post := func(body string) int {
		resp, err := h.srv.Client().Post(h.srv.URL+protocol.PathWorkspaces, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	for name, body := range map[string]string{
		"relative":     `{"workdir":"rel/dir"}`,
		"missing":      `{"workdir":"` + filepath.Join(t.TempDir(), "nope") + `"}`,
		"not a folder": `{"workdir":"` + file + `"}`,
	} {
		if got := post(body); got != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, got)
		}
	}
}

func TestSandboxedRequestRefusals(t *testing.T) {
	h := sandboxHarness(t, nil)
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	create := func(ws, target string, exe *core.Execution) error {
		_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: target, Prompt: "hi", Options: core.SessionOptions{Execution: exe}})
		return err
	}
	var perr *protocol.ProtocolError
	expect := func(name string, err error, want string) {
		t.Helper()
		if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, want) {
			t.Errorf("%s = %v, want a bad_request containing %q", name, err, want)
		}
	}
	expect("a host session in a sandbox workspace", create(ws, "fake", nil), "takes only sandboxed sessions")
	plain, _ := h.workspace()
	expect("a sandboxed session in a host workspace", create(plain, "fake", sbExecution(dir, vmexec, "t", "sp")), "workspace created in the sandbox workdir")
	expect("another workdir", create(ws, "fake", sbExecution(t.TempDir(), vmexec, "t", "sp")), "is not this workspace")
	expect("a helper that is not a file", create(ws, "fake", sbExecution(dir, "/no/such/vmexec", "t", "sp")), "not an absolute path to a file")
	other := sbExecution(dir, vmexec, "t", "sp")
	other.Sandbox.HostConfine.Workspace = t.TempDir()
	expect("a confinement for another workspace", create(ws, "fake", other), "host confinement's workspace")

	// An agent without a routing is refused, by name, and nothing starts.
	cfg := fakeAgentConfig(t, "plain", nil)
	h2 := newHarness(t, Config{Agents: []AgentConfig{cfg}})
	ws2 := h2.workdirWorkspace(t.TempDir())
	w2, _ := h2.a.ws.get(ws2)
	_, err := h2.client.CreateSession(h2.ctx(), h2.ep, core.SessionRequest{WorkspaceID: ws2, TargetID: "plain", Prompt: "hi",
		Options: core.SessionOptions{Execution: sbExecution(w2.real, vmexec, "t", "sp")}})
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "plain") || !strings.Contains(perr.Message, "`sandbox`") {
		t.Fatalf("an agent without a routing = %v", err)
	}
}

// The agent is started through vmexec confine in the workdir with the
// descriptor's environment: the token only as a file, the adapter's own
// settings not at all, the terminals not offered, and the routing's config
// write-denied.
func TestSandboxedSessionStartsConfinedWithTheDescriptor(t *testing.T) {
	h := sandboxHarness(t, nil)
	t.Setenv("BNBLOC_ACP_TOKEN", "adapter-only")
	vmexec, logPath := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "token-1", "space-a")
	sid := h.sandboxSession(ws, exe)
	in := h.readInfo(ws, sid)

	real, _ := filepath.EvalSymlinks(dir)
	if cwd, _ := filepath.EvalSymlinks(in.Cwd); cwd != real || in.Pwd != dir && in.Pwd != real {
		t.Fatalf("cwd=%q pwd=%q, want the workdir %q", in.Cwd, in.Pwd, dir)
	}
	if raw, _ := os.ReadFile(logPath); !strings.Contains("\n"+string(raw), "\n--deny-read\n/h/.config/acme/mail\n") {
		t.Fatalf("the descriptor's denyRead did not reach confine as a flag: %q", raw)
	}
	if in.Space != "space-a" || in.DenyRead != `["/h/.config/acme/mail"]` {
		t.Fatalf("descriptor in the environment: space=%q denyRead=%q", in.Space, in.DenyRead)
	}
	if in.TokenEnv != "" || in.Token != "token-1" || in.TokenFile == "" {
		t.Fatalf("token: env=%q file=%q (%q): it is the file's alone", in.TokenEnv, in.Token, in.TokenFile)
	}
	if in.Terminal {
		t.Fatal("the client terminals were offered to a sandboxed agent: their commands would run on the host")
	}
	if in.ACPEnvLeak {
		t.Fatal("the adapter's own settings reached the agent")
	}
	if in.Config == "" || !strings.Contains(in.Config, "opencode-") {
		t.Fatalf("OPENCODE_CONFIG_DIR = %q", in.Config)
	}
	if _, err := os.Stat(filepath.Join(in.Config, "plugin", "bnbloc-vmexec.js")); err != nil {
		t.Fatalf("the routing plugin was not written: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(in.Config, "plugin", "bnbloc-vmexec.js")); !strings.Contains(string(raw), `"`+vmexec+`"`) || !strings.Contains(string(raw), "hook") {
		t.Fatalf("plugin = %s", raw)
	}

	raw, _ := os.ReadFile(logPath)
	args := strings.Split(strings.TrimSpace(strings.Split(string(raw), "---")[0]), "\n")
	if args[0] != "confine" {
		t.Fatalf("vmexec was called as %v", args)
	}
	tokenDir := filepath.Dir(in.TokenFile)
	joined := "\n" + strings.Join(args, "\n") + "\n"
	for _, want := range []string{
		"\n--harness\nopencode\n--harness\nclaude\n", "\n--deny-read\n" + h.home + "\n", "\n--allow-read\n" + tokenDir + "\n",
		"\n--deny-write\n" + tokenDir + "\n", "\n--allow-read\n" + in.Config + "\n", "\n--deny-write\n" + in.Config + "\n", "\n--\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("confine lacks %q in %v", want, args)
		}
	}
}

// The token is renewed in its file at every input and the agent process keeps
// running: it never holds a stale one.
func TestSandboxedInputRenewsTheTokenInTheSameProcess(t *testing.T) {
	h := sandboxHarness(t, nil)
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "token-1", "space-a"))
	first := h.readInfo(ws, sid)
	h.sendSandboxed(ws, sid, "sandboxinfo", sbExecution(dir, vmexec, "token-2", "space-a"))
	second := h.readInfo(ws, sid)
	if first.Token != "token-1" || second.Token != "token-2" {
		t.Fatalf("tokens = %q then %q", first.Token, second.Token)
	}
	if first.PID != second.PID {
		t.Fatalf("a renewed token restarted the agent (%d then %d)", first.PID, second.PID)
	}
}

// A changed descriptor (another space) continues the same ACP session in a new
// process; an agent that cannot resume or load sessions refuses the input.
func TestSandboxedDescriptorChangeContinuesInANewProcess(t *testing.T) {
	h := sandboxHarness(t, map[string]string{"ACP_FAKE_RESUME": "1"})
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t1", "space-a"))
	first := h.readInfo(ws, sid)
	h.sendSandboxed(ws, sid, "sandboxinfo", sbExecution(dir, vmexec, "t2", "space-b"))
	second := h.readInfo(ws, sid)
	if second.Space != "space-b" || second.PID == first.PID {
		t.Fatalf("after a descriptor change: space %q pid %d (was %d)", second.Space, second.PID, first.PID)
	}

	stuck := sandboxHarness(t, nil) // cannot resume or load
	ws2 := stuck.workdirWorkspace(dir)
	sid2 := stuck.sandboxSession(ws2, sbExecution(dir, vmexec, "t1", "space-a"))
	stuck.readInfo(ws2, sid2)
	stuck.sendSandboxed(ws2, sid2, "sandboxinfo", sbExecution(dir, vmexec, "t2", "space-b"))
	ts := stuck.settle(ws2, sid2)
	if !ts.Failed || !strings.Contains(ts.FailMessage, "cannot continue its session") {
		t.Fatalf("an agent that cannot continue kept running under the old scope: %+v", ts)
	}
}

// A session runs where it started.
func TestASessionKeepsItsPlace(t *testing.T) {
	h := sandboxHarness(t, nil)
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t", "sp"))
	h.settle(ws, sid)
	var perr *protocol.ProtocolError
	if err := h.client.SendInput(h.ctx(), h.ep, ws, sid, "hi", core.SessionOptions{}); !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
		t.Fatalf("a host input to a sandboxed session = %v", err)
	}
	plain, _ := h.workspace()
	hs := h.session(plain, "fake", "hello", core.SessionOptions{})
	h.settle(plain, hs)
	if err := h.client.SendInput(h.ctx(), h.ep, plain, hs, "hi", core.SessionOptions{Execution: sbExecution(dir, vmexec, "t", "sp")}); !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
		t.Fatalf("a sandboxed input to a host session = %v", err)
	}
}

// Closing the session removes its token file.
func TestSandboxedSessionTokenIsRemovedWithTheWorkspace(t *testing.T) {
	h := sandboxHarness(t, nil)
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "secret-token", "sp"))
	in := h.readInfo(ws, sid)
	if _, err := os.Stat(in.TokenFile); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(in.TokenFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v", st.Mode().Perm())
	}
	if err := h.client.DeleteWorkspace(h.ctx(), h.ep, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(in.TokenFile)); !os.IsNotExist(err) {
		t.Fatalf("the token directory outlived its session: %v", err)
	}
}

// A routing that cannot be set up refuses the session: it never starts on the host.
func TestSandboxedRoutingFailureRefusesTheSession(t *testing.T) {
	h := sandboxHarness(t, nil)
	warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error {
		return refuse("opencode was not found on PATH")
	}
	vmexec, logPath := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hi", Options: core.SessionOptions{Execution: sbExecution(dir, vmexec, "t", "sp")}})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "opencode was not found") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(logPath); len(raw) != 0 {
		t.Fatalf("the agent was started anyway: %s", raw)
	}
}

// The real thing: the agent is started by the real `bnbloc-vmexec confine`
// and the OS sandbox holds. It writes its workspace; it cannot write outside,
// read the adapter's home (the agents' config, every other workspace), or
// change its token file. Skipped where the helper cannot be built or the
// sandbox cannot be applied.
func TestRealConfineHoldsForTheAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("run with the real Seatbelt profile (macOS)")
	}
	needSandboxHere(t)
	bin := filepath.Join(t.TempDir(), "bnbloc-vmexec")
	if out, err := execCommand(t, "go", "build", "-o", bin, "github.com/luckylhb90/bnbloc/cmd/bnbloc-vmexec"); err != nil {
		t.Skipf("the sandbox helper cannot be built here: %v\n%s", err, out)
	}
	// The adapter's home is outside the temp directories (which a confined agent
	// may read and write): where it is in real use, beside the user's other data.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	acpHome, err := os.MkdirTemp(userHome, ".bnbloc-acp-confine-test-")
	if err != nil {
		t.Skipf("no place for the adapter's home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(acpHome) })
	h := sandboxHarnessAt(t, nil, acpHome)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	outside := filepath.Join(userHome, ".bnbloc-acp-confine-probe")
	t.Cleanup(func() { os.Remove(outside) })
	homeFile := filepath.Join(h.home, "agents.json")
	exe := sbExecution(dir, bin, "real-token", "sp")
	exe.Sandbox.HostConfine.DenyRead = nil
	exe.Sandbox.HostConfine.DenyWrite = nil
	sid := h.sandboxSession(ws, exe)
	first := h.readInfo(ws, sid)
	if first.Token != "real-token" {
		t.Fatalf("the confined agent did not start as described: %+v", first)
	}
	h.sendSandboxed(ws, sid, "fsprobe "+dir+"|"+outside+"|"+homeFile+"|"+first.TokenFile, exe)
	h.settle(ws, sid)
	var res map[string]string
	if err := json.Unmarshal([]byte(h.lastText(ws, sid)), &res); err != nil {
		t.Fatalf("probe = %q: %v", h.lastText(ws, sid), err)
	}
	if res["writeWorkspace"] != "ok" {
		t.Errorf("the agent cannot write its own workspace: %v", res)
	}
	if res["readToken"] != "ok" {
		t.Errorf("the agent cannot read its token directory (vmexec could not either): %v", res)
	}
	for _, denied := range []string{"writeOutside", "readHome", "writeToken"} {
		if res[denied] == "ok" {
			t.Errorf("%s succeeded under confinement: %v", denied, res)
		}
	}
	if raw, _ := os.ReadFile(first.TokenFile); string(raw) != "real-token" {
		t.Errorf("the token file was changed by the agent: %q", raw)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Errorf("a file was written outside the workspace")
	}
}

func execCommand(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The member can see which agents can run sandboxed, and why not, before a
// turn is tried: in `check` and on the agents status route.
func TestCheckAndStatusSaySandboxSupport(t *testing.T) {
	needSandboxHere(t)
	good := fakeAgentConfig(t, "routed", nil)
	good.Sandbox = sandboxRoutingOpencode
	plain := fakeAgentConfig(t, "plain", nil)
	home := t.TempDir()
	raw, _ := json.Marshal(Config{Agents: []AgentConfig{good, plain}})
	cfgPath := filepath.Join(home, "agents.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := check(options{home: home, config: cfgPath, name: "acp"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	var routed, notRouted string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "routed"):
			routed = l
		case strings.HasPrefix(l, "plain"):
			notRouted = l
		}
	}
	if !strings.Contains(out.String(), "SANDBOX") || !strings.Contains(routed, "yes (opencode)") || !strings.Contains(notRouted, "no: ") || !strings.Contains(notRouted, "`sandbox`") {
		t.Fatalf("check =\n%s", out.String())
	}

	h := sandboxHarness(t, nil)
	var statuses []agentStatus
	for _, st := range h.a.reg.statuses() {
		statuses = append(statuses, st)
	}
	if len(statuses) != 1 || statuses[0].Sandbox != sandboxRoutingOpencode || statuses[0].SandboxNote != "" {
		t.Fatalf("status = %+v", statuses)
	}
}

// The real agent, the real helper, the real routing (opt in: they start the
// installed agent with its login and make small model calls). The agent starts
// confined with the routing loaded and answers a turn, and a command it is asked
// to run through its bash tool does NOT run on the host: the routing hands it to
// vmexec, which has no sandbox service to run it in here, so it fails and leaves
// no marker. No `sandbox` setting is given: the routing is recognised from the
// command.
func realConfinedAndRouted(t *testing.T, agent AgentConfig, bashTool string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the real Seatbelt profile (macOS)")
	}
	bin := filepath.Join(t.TempDir(), "bnbloc-vmexec")
	if out, err := execCommand(t, "go", "build", "-o", bin, "github.com/luckylhb90/bnbloc/cmd/bnbloc-vmexec"); err != nil {
		t.Fatalf("build the helper: %v\n%s", err, out)
	}
	userHome, _ := os.UserHomeDir()
	acpHome, err := os.MkdirTemp(userHome, ".bnbloc-acp-real-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(acpHome) })
	// No permissions setting either: a member's config has none.
	h := newHarnessAt(t, Config{Agents: []AgentConfig{agent}}, acpHome)
	if st := h.a.reg.statuses(); len(st) != 1 || !st[0].Available || st[0].Sandbox == "" {
		t.Fatalf("%s is not available (or not sandbox-capable) through the adapter: %+v", agent.Name, st)
	}
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, bin, "unused-token", "real-space")
	exe.Sandbox.URL = "http://127.0.0.1:9" // nothing listens: a routed command cannot run
	exe.Sandbox.HostConfine.DenyRead, exe.Sandbox.HostConfine.DenyWrite = nil, nil

	wait := func(sid string) core.TurnStatus {
		deadline := time.Now().Add(6 * time.Minute)
		for time.Now().Before(deadline) {
			ts, err := h.client.PollTurn(context.Background(), h.ep, ws, sid)
			if err != nil {
				t.Fatalf("PollTurn: %v", err)
			}
			if ts.Idle {
				return ts
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatal("the turn never ended")
		return core.TurnStatus{}
	}
	var res core.SessionResult
	for attempt := 0; ; attempt++ { // the first start may download the agent into its private cache
		res, err = h.client.CreateSession(context.Background(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: agent.Name,
			Prompt:  "Reply with exactly the word ok and nothing else. Do not use any tools.",
			Options: core.SessionOptions{Execution: exe}})
		if err == nil {
			break
		}
		if attempt >= 3 {
			t.Fatalf("CreateSession: %v", err)
		}
		t.Logf("CreateSession attempt %d: %v", attempt+1, err)
	}
	if ts := wait(res.SessionID); ts.Failed {
		t.Fatalf("the first turn failed: %+v", ts)
	}
	msgs, _, _ := h.client.Messages(context.Background(), h.ep, ws, res.SessionID, 0)
	t.Logf("first reply: %q", texts(msgs))

	marker := filepath.Join(dir, "HOST_RAN.txt")
	if err := h.client.SendInput(context.Background(), h.ep, ws, res.SessionID,
		"Use your "+bashTool+" tool to run exactly this command and then tell me what happened: echo ran > "+marker,
		core.SessionOptions{Execution: exe}); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	ts := wait(res.SessionID)
	msgs, _, _ = h.client.Messages(context.Background(), h.ep, ws, res.SessionID, 0)
	t.Logf("second turn: failed=%v replies=%q", ts.Failed, texts(msgs))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran on the host: the routing did not take the shell call")
	}
}

func TestRealOpencodeIsConfinedAndRouted(t *testing.T) {
	if os.Getenv("ACP_REAL_OPENCODE") != "1" {
		t.Skip("set ACP_REAL_OPENCODE=1 to run the installed opencode")
	}
	realConfinedAndRouted(t, AgentConfig{Name: "opencode", Command: "opencode", Args: []string{"acp"}}, "bash")
}

func TestRealClaudeIsConfinedAndRouted(t *testing.T) {
	if os.Getenv("ACP_REAL_CLAUDE") != "1" {
		t.Skip("set ACP_REAL_CLAUDE=1 to run the Claude ACP agent through npx")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx is not on PATH")
	}
	realConfinedAndRouted(t, AgentConfig{Name: "claude", Command: npx, Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}}, "Bash")
}

// The routing is recognised from the agent's command, as vmexec recognises a
// harness from the executable: no setting is needed, and an explicit one
// overrides.
func TestSandboxRoutingIsRecognisedFromTheCommand(t *testing.T) {
	for _, c := range []struct {
		agent       AgentConfig
		routing     string
		noteHas     string
		explanation string
	}{
		{AgentConfig{Command: "/Users/m/.opencode/bin/opencode", Args: []string{"acp"}}, "opencode", "", "opencode by path"},
		{AgentConfig{Command: "opencode"}, "opencode", "", "opencode on PATH"},
		{AgentConfig{Command: "/x/bin/npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}}, "claude", "", "claude through npx"},
		{AgentConfig{Command: "npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp@1.2.3"}}, "claude", "", "claude, a pinned version"},
		{AgentConfig{Command: "claude-code-acp"}, "claude", "", "claude by its other name"},
		{AgentConfig{Command: "npx", Args: []string{"-y", "@agentclientprotocol/codex-acp"}}, "codex", "", "codex through npx"},
		{AgentConfig{Command: "gemini", Args: []string{"--acp"}}, "", "no sandbox routing is known", "an unknown agent"},
		{AgentConfig{Command: "gemini", Sandbox: "opencode"}, "opencode", "", "an explicit routing wins"},
		{AgentConfig{Command: "opencode", Sandbox: "off"}, "", "`sandbox` to off", "off turns it off"},
	} {
		routing, note := c.agent.sandboxRouting()
		if routing != c.routing || (c.noteHas == "") != (note == "") || !strings.Contains(note, c.noteHas) {
			t.Errorf("%s: routing=%q note=%q, want %q / %q", c.explanation, routing, note, c.routing, c.noteHas)
		}
	}
}

// A claude agent gets Claude Code's shell-prefix router: a link, named for the
// router, to the helper, in the directory the agent can read and never write;
// and its own npm cache, because npx would otherwise fail on npm's.
func TestClaudeRoutingAndThePrivateNpmCache(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingClaude
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, logPath := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t", "sp"))
	in := h.readInfo(ws, sid)
	if filepath.Base(in.ClaudePrefix) != "bnbloc-vmexec-claude-prefix" || filepath.Dir(in.ClaudePrefix) != filepath.Dir(in.TokenFile) {
		t.Fatalf("CLAUDE_CODE_SHELL_PREFIX = %q (token file %q)", in.ClaudePrefix, in.TokenFile)
	}
	if target, err := os.Readlink(in.ClaudePrefix); err != nil || target != vmexec {
		t.Fatalf("the prefix link goes to %q, %v; want the helper %q", target, err, vmexec)
	}
	if in.Config != "" {
		t.Fatalf("a claude agent got opencode's config directory: %q", in.Config)
	}
	if !strings.Contains(in.NPMCache, "npm-fake") || strings.HasPrefix(in.NPMCache, os.Getenv("HOME")+"/.npm") {
		t.Fatalf("npm_config_cache = %q", in.NPMCache)
	}
	raw, _ := os.ReadFile(logPath)
	joined := "\n" + strings.Join(strings.Split(strings.TrimSpace(strings.Split(string(raw), "---")[0]), "\n"), "\n") + "\n"
	for _, want := range []string{"\n--harness\nclaude\n", "\n--allow-write\n" + in.NPMCache + "\n", "\n--allow-read\n" + in.NPMCache + "\n"} {
		if !strings.Contains(joined, want) {
			t.Errorf("confine lacks %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "\n--harness\nopencode\n") {
		t.Errorf("a claude agent was given opencode's profile: %q", joined)
	}
}

// fakeVMExecCodex is fakeVMExec that also answers the space's `command -v codex`
// (found or not) and the exec-server `run --stdio`, which it echoes line by line.
func fakeVMExecCodex(t *testing.T, haveCodex bool) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	path = filepath.Join(dir, "bnbloc-vmexec")
	found := "exit 1"
	if haveCodex {
		found = "exit 0"
	}
	script := `#!/bin/sh
{ printf '%s\n' "$@"; echo '---'; } >> "` + log + `"
if [ "$1" = run ] && [ "$2" = -- ]; then ` + found + `; fi
if [ "$1" = run ]; then exec cat; fi
[ "$1" = confine ] || exit 64
shift
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

// A codex agent runs with its own profile alone and a remote executor that is a
// bridge into the space: the URL is loopback, it is served while the agent
// lives, and a connection to it runs the exec-server in the space (not on the
// host) through the helper. The space is asked first whether it has codex.
func TestCodexRoutingBridgesItsExecutorIntoTheSpace(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingCodex
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, logPath := fakeVMExecCodex(t, true)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t", "sp"))
	in := h.readInfo(ws, sid)
	u, err := url.Parse(in.ExecServer)
	if err != nil || u.Scheme != "ws" || !strings.HasPrefix(u.Host, "127.0.0.1:") || len(u.Path) < 16 {
		t.Fatalf("CODEX_EXEC_SERVER_URL = %q, %v", in.ExecServer, err)
	}
	c, _, err := websocket.Dial(h.ctx(), in.ExecServer, nil)
	if err != nil {
		t.Fatalf("dial the bridge: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(h.ctx(), websocket.MessageText, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.Read(h.ctx()); err != nil || strings.TrimSpace(string(got)) != `{"id":1}` {
		t.Fatalf("echoed through the space: %q, %v", got, err)
	}
	raw, _ := os.ReadFile(logPath)
	joined := "\n" + string(raw)
	for _, want := range []string{"\nrun\n--\ncommand -v codex\n", "\nrun\n--stdio\n--\ncodex\nexec-server\n--listen\nstdio\n", "\n--harness\ncodex\n"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the helper was not called with %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "\n--harness\nclaude\n") || strings.Contains(joined, "\n--harness\nopencode\n") {
		t.Errorf("a codex agent was given another agent's profile: %q", joined)
	}
}

// The bridge ends with the agent: no listener outlives the process it served.
func TestCodexBridgeEndsWithTheAgent(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingCodex
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, _ := fakeVMExecCodex(t, true)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t", "sp"))
	in := h.readInfo(ws, sid)
	u, _ := url.Parse(in.ExecServer)
	if conn, err := net.DialTimeout("tcp", u.Host, time.Second); err != nil {
		t.Fatalf("the bridge is not served while the agent lives: %v", err)
	} else {
		conn.Close()
	}
	h.a.close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", u.Host, 200*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the bridge still listens after the agent stopped")
}

// A space without codex is a refusal that says so, before the agent starts.
func TestCodexRoutingRefusesASpaceWithoutCodex(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingCodex
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, _ := fakeVMExecCodex(t, false)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hi", Options: core.SessionOptions{Execution: sbExecution(dir, vmexec, "t", "sp")}})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "codex is not installed") {
		t.Fatalf("err = %v", err)
	}
}

func TestSandboxOffRefusesTheAgent(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxOff
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hi", Options: core.SessionOptions{Execution: sbExecution(dir, vmexec, "t", "sp")}})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "off") {
		t.Fatalf("err = %v", err)
	}
}

// The point of the sandbox is that an agent runs commands: in a sandboxed
// session its command execution is approved with no setting (its commands are
// routed into the space), unlike a host session, where `execute` stays off
// unless the operator allows it. An operator's own list is respected.
func TestSandboxedSessionApprovesCommandsByDefault(t *testing.T) {
	needSandboxHere(t)
	old := warmOpencodeFn
	warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error { return nil }
	t.Cleanup(func() { warmOpencodeFn = old })
	vmexec, _ := fakeVMExec(t)
	ask := func(agent AgentConfig, sandboxed bool) string {
		t.Helper()
		h := newHarness(t, Config{Agents: []AgentConfig{agent}})
		dir := t.TempDir()
		var ws, sid string
		if sandboxed {
			ws = h.workdirWorkspace(dir)
			sid = h.sandboxSession(ws, sbExecution(dir, vmexec, "t", "sp"))
			h.readInfo(ws, sid)
			h.sendSandboxed(ws, sid, "permission execute "+dir, sbExecution(dir, vmexec, "t", "sp"))
		} else {
			var root workspaceRoot
			ws, root = h.workspace()
			sid = h.session(ws, "fake", "hello", core.SessionOptions{})
			h.settle(ws, sid)
			h.send(ws, sid, "permission execute "+root.dir, core.SessionOptions{})
		}
		h.settle(ws, sid)
		return h.lastText(ws, sid)
	}
	routed := fakeAgentConfig(t, "fake", nil)
	routed.Sandbox = sandboxRoutingOpencode
	if got := ask(routed, true); !strings.Contains(got, `"optionId":"yes"`) {
		t.Errorf("a sandboxed agent's command was refused by default: %s", got)
	}
	if got := ask(fakeAgentConfig(t, "fake", nil), false); !strings.Contains(got, `"optionId":"no"`) {
		t.Errorf("a host session approved a command with no setting: %s", got)
	}
	explicit := routed
	explicit.Permissions = Permissions{Allow: []string{"read", "edit"}}
	if got := ask(explicit, true); !strings.Contains(got, `"optionId":"no"`) {
		t.Errorf("an operator's own list was overridden: %s", got)
	}
}

// The adapter is not in the sandbox, so it must not do for a confined agent what
// the OS sandbox forbids the agent: a rogue agent asks for a terminal and for a
// write whether or not it was offered them.
func TestSandboxedSessionIsRefusedWhatTheOSSandboxWouldRefuse(t *testing.T) {
	h := sandboxHarness(t, nil)
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "t", "sp")
	exe.Sandbox.HostConfine.DenyWrite = append(exe.Sandbox.HostConfine.DenyWrite, filepath.Join(dir, ".routing")+"/")
	sid := h.sandboxSession(ws, exe)
	h.readInfo(ws, sid)
	ask := func(prompt string) string {
		t.Helper()
		h.sendSandboxed(ws, sid, prompt, exe)
		h.settle(ws, sid)
		return h.lastText(ws, sid)
	}
	marker := filepath.Join(t.TempDir(), "ran-on-the-host")
	// The create itself is refused (the fake agent reports a failed create as
	// "terminal=<error>"; a served one goes on to "exit=...").
	if got := ask("terminal touch " + marker); !strings.HasPrefix(got, "terminal=") || !strings.Contains(got, "sandboxed session") {
		t.Errorf("a terminal was created in a sandboxed session: %s", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a command ran on the host through the adapter's terminal")
	}
	for _, rel := range []string{".claude/settings.json", "opencode.json", ".codex/config.toml", "CLAUDE.local.md", ".routing/x.txt"} {
		got := ask("write " + filepath.Join(dir, rel) + " planted")
		if strings.Contains(got, `"result"`) || !strings.Contains(got, "write=") || !strings.Contains(got, "error") {
			t.Errorf("a write to %s was not refused: %s", rel, got)
		}
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			t.Errorf("%s was written through the adapter", rel)
		}
	}
	if got := ask("write " + filepath.Join(dir, "out", "report.md") + " fine"); strings.Contains(got, `"error"`) {
		t.Errorf("an ordinary write was refused: %s", got)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "out", "report.md")); err != nil || string(raw) != "fine" {
		t.Errorf("the ordinary write did not land: %q %v", raw, err)
	}
}

// A session that never started leaves no exec token behind, and a restart sweeps
// what a crash left.
func TestExecTokensDoNotOutliveTheirSession(t *testing.T) {
	h := sandboxHarness(t, nil)
	warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error { return refuse("opencode was not found") }
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	if _, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hi",
		Options: core.SessionOptions{Execution: sbExecution(dir, vmexec, "secret-token", "sp")}}); err == nil {
		t.Fatal("the create did not fail")
	}
	left, _ := os.ReadDir(filepath.Join(h.a.sandbox.stateDir, "sessions"))
	if len(left) != 0 {
		t.Fatalf("a failed create left its token: %v", left)
	}
	// A crash's leftover is swept at start.
	stale := filepath.Join(h.a.sandbox.stateDir, "sessions", "s-crashed")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(stale, "token"), []byte("old"), 0o600)
	h.a.sandbox.sweep()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the sweep left %s: %v", stale, err)
	}
}

// A package of one space is not another's: one npm cache per sandbox space.
func TestNpmCacheIsPerSpace(t *testing.T) {
	needSandboxHere(t)
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingClaude
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	vmexec, _ := fakeVMExec(t)
	cache := func(space string) string {
		dir := t.TempDir()
		ws := h.workdirWorkspace(dir)
		sid := h.sandboxSession(ws, sbExecution(dir, vmexec, "t", space))
		return h.readInfo(ws, sid).NPMCache
	}
	one, two, again := cache("space-one"), cache("space-two"), cache("space-one")
	if one == two || one != again {
		t.Fatalf("npm caches: %q %q %q; want one per space, stable for the same space", one, two, again)
	}
}

// An argument is not a command: only the executable, or the package a launcher
// is given, names a routing.
func TestRoutingIsNotGuessedFromUnrelatedArguments(t *testing.T) {
	for _, a := range []AgentConfig{
		{Command: "gemini", Args: []string{"--config", "/x/opencode"}},
		{Command: "my-agent", Args: []string{"@agentclientprotocol/claude-agent-acp"}},
		{Command: "node", Args: []string{"/x/claude-agent-acp"}},
	} {
		if routing, _ := a.sandboxRouting(); routing != "" {
			t.Errorf("%s %v was given the %s routing and would run confined but unrouted", a.Command, a.Args, routing)
		}
	}
	for _, a := range []AgentConfig{
		{Command: "npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp@1.2.3"}},
		{Command: "/opt/bin/pnpx", Args: []string{"opencode-ai", "acp"}},
	} {
		if routing, _ := a.sandboxRouting(); routing == "" {
			t.Errorf("%s %v was not recognised", a.Command, a.Args)
		}
	}
}

// A relative --home would make every sandboxed start fail (the helper refuses a
// relative path), so it is made absolute.
func TestHomeIsAbsolute(t *testing.T) {
	o, err := parseOptions("serve", []string{"--home", "rel/home"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(o.home) {
		t.Fatalf("home = %q", o.home)
	}
}

// A path an agent's config (or a component's drop-in, for every agent) keeps
// from confined runs reaches the confining helper as a flag and in the
// descriptor's environment, joined to the descriptor's own; a relative path is
// refused; and a drop-in can only restrict.
func TestDenyReadFromTheConfigJoinsTheDescriptors(t *testing.T) {
	needSandboxHere(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, dropInDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A component's drop-in names a credential store for every agent; the
	// member's own entry has its own path.
	if err := os.WriteFile(filepath.Join(home, dropInDir, "component.json"), []byte(`{"denyRead":["/h/.component/login"],"agents":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := warmOpencodeFn
	warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error { return nil }
	t.Cleanup(func() { warmOpencodeFn = old })
	a := fakeAgentConfig(t, "fake", nil)
	a.Sandbox = sandboxRoutingOpencode
	a.DenyRead = []string{"/h/.own/secrets"}
	h := newHarnessAt(t, Config{Agents: []AgentConfig{a}}, home)
	vmexec, logPath := fakeVMExec(t)
	dir := t.TempDir()
	ws := h.workdirWorkspace(dir)
	exe := sbExecution(dir, vmexec, "t", "space-a")
	sid := h.sandboxSession(ws, exe)
	in := h.readInfo(ws, sid)
	raw, _ := os.ReadFile(logPath)
	joined := "\n" + string(raw)
	for _, p := range []string{"/h/.config/acme/mail", "/h/.own/secrets", "/h/.component/login"} {
		if !strings.Contains(joined, "\n--deny-read\n"+p+"\n") {
			t.Errorf("confine lacks --deny-read %s:\n%s", p, raw)
		}
		if !strings.Contains(in.DenyRead, p) {
			t.Errorf("the descriptor's environment lacks %s: %q", p, in.DenyRead)
		}
	}
	if _, err := ParseConfig([]byte(`{"agents":[{"name":"a","command":"x","denyRead":["relative/path"]}]}`)); err == nil {
		t.Error("a relative denyRead was accepted")
	}
	if _, err := ParseConfig([]byte(`{"denyRead":["relative"],"agents":[]}`)); err == nil {
		t.Error("a relative core-wide denyRead was accepted")
	}
}

// The real helper: a file the config keeps from confined runs is read by the
// agent without the setting and not with it. (The path is a dot directory under
// the home, which the confinement reads by default.)
func TestRealConfineHonoursDenyRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the real Seatbelt profile (macOS)")
	}
	needSandboxHere(t)
	bin := filepath.Join(t.TempDir(), "bnbloc-vmexec")
	if out, err := execCommand(t, "go", "build", "-o", bin, "github.com/luckylhb90/bnbloc/cmd/bnbloc-vmexec"); err != nil {
		t.Skipf("the sandbox helper cannot be built here: %v\n%s", err, out)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	secretDir, err := os.MkdirTemp(userHome, ".bnbloc-acp-denyread-test-")
	if err != nil {
		t.Skipf("no place for the probe: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(secretDir) })
	secret := filepath.Join(secretDir, "login")
	if err := os.WriteFile(secret, []byte("a credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		deny []string
		want string
	}{{nil, "readable"}, {[]string{secretDir}, "denied"}} {
		acpHome, err := os.MkdirTemp(userHome, ".bnbloc-acp-denyread-home-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(acpHome) })
		old := warmOpencodeFn
		warmOpencodeFn = func(context.Context, *sandboxLaunch, []string, string) error { return nil }
		a := fakeAgentConfig(t, "fake", nil)
		a.Sandbox = sandboxRoutingOpencode
		a.DenyRead = c.deny
		h := newHarnessAt(t, Config{Agents: []AgentConfig{a}}, acpHome)
		dir := t.TempDir()
		ws := h.workdirWorkspace(dir)
		exe := sbExecution(dir, bin, "t", "sp")
		exe.Sandbox.HostConfine.DenyRead, exe.Sandbox.HostConfine.DenyWrite = nil, nil
		sid := h.sandboxSession(ws, exe)
		h.sendSandboxed(ws, sid, "readable "+secret, exe)
		h.settle(ws, sid)
		if got := h.lastText(ws, sid); got != c.want {
			t.Errorf("denyRead %v: the confined agent's read of the file = %q, want %q", c.deny, got, c.want)
		}
		warmOpencodeFn = old
	}
}

// An agent with cleanEnv runs confined without the credentials the adapter
// inherited (its network is open: each is one it could send away), and keeps
// what the member's config, the provider and the descriptor give it. An agent
// without it inherits as before, and a host run is never cleaned.
func TestCleanEnvKeepsInheritedCredentialsFromAConfinedAgent(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "inherited-cloud-secret")
	t.Setenv("ACME_API_TOKEN", "inherited-token")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("ACME_PLAIN_SETTING", "plain")
	cfg, _, _ := fakeProvider(t, []string{"PROVIDED_API_TOKEN=from-provider"}, "")
	read := func(clean, sandboxed bool, name string) string {
		h := sandboxHarness(t, map[string]string{"EXPLICIT_API_TOKEN": "from-config"})
		h.a.reg.cfg.Agents[0].CleanEnv = clean
		h.a.reg.cfg.Agents[0].EnvProvider = cfg
		if !sandboxed {
			ws, _ := h.workspace()
			sid := h.session(ws, "fake", "hasenv "+name, core.SessionOptions{})
			h.settle(ws, sid)
			return strings.TrimPrefix(h.lastText(ws, sid), "set:")
		}
		dir := t.TempDir()
		ws := h.workdirWorkspace(dir)
		vmexec, _ := fakeVMExec(t)
		exe := sbExecution(dir, vmexec, "token-1", "space-a")
		sid := h.sandboxSession(ws, exe)
		h.sendSandboxed(ws, sid, "hasenv "+name, exe)
		h.settle(ws, sid)
		return strings.TrimPrefix(h.lastText(ws, sid), "set:")
	}
	for _, name := range []string{"AWS_SECRET_ACCESS_KEY", "ACME_API_TOKEN", "SSH_AUTH_SOCK"} {
		if got := read(true, true, name); got != "unset" {
			t.Errorf("cleanEnv: %s reached the confined agent (%q)", name, got)
		}
		if got := read(false, true, name); got == "unset" {
			t.Errorf("without cleanEnv: %s was dropped though nobody asked", name)
		}
		if got := read(true, false, name); got == "unset" {
			t.Errorf("cleanEnv: %s was dropped from a run on the host", name)
		}
	}
	for name, want := range map[string]string{"ACME_PLAIN_SETTING": "plain", "EXPLICIT_API_TOKEN": "from-config", "PROVIDED_API_TOKEN": "from-provider", "BNBLOC_VMEXEC_SPACE": "space-a"} {
		if got := read(true, true, name); got != want {
			t.Errorf("cleanEnv: %s = %q in the confined agent, want %q", name, got, want)
		}
	}
}

func TestCredentialEnvNames(t *testing.T) {
	for _, n := range []string{"JIRA_TOKEN", "OPENAI_API_KEY", "AWS_PROFILE", "GH_TOKEN", "NODE_AUTH_TOKEN", "DB_PASSWORD", "SSH_AUTH_SOCK", "GIT_CONFIG_KEY_0", "GIT_CONFIG_COUNT", "GOOGLE_APPLICATION_CREDENTIALS", "private_key"} {
		if !credentialEnvName(n) {
			t.Errorf("%s is not judged a credential", n)
		}
	}
	for _, n := range []string{"PATH", "HOME", "LANG", "TMPDIR", "CLAUDE_CODE_MAX_OUTPUT_TOKENS", "MAX_THINKING_TOKENS", "KEYBOARD_LAYOUT", "OPENCODE_CONFIG_DIR", "ANTHROPIC_BASE_URL", "BNBLOC_VMEXEC_SPACE"} {
		if credentialEnvName(n) {
			t.Errorf("%s is judged a credential", n)
		}
	}
}

// The real agent's package, damaged after its install step finished (its platform
// binary cut short, as a cut-off install leaves it), heals: the start that fails
// makes the entry untrusted and the next one replaces it.
func TestRealClaudeHealsADamagedCache(t *testing.T) {
	if os.Getenv("ACP_REAL_CLAUDE") != "1" {
		t.Skip("set ACP_REAL_CLAUDE=1 to run the Claude ACP agent through npx")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the real Seatbelt profile (macOS)")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx is not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "bnbloc-vmexec")
	if out, err := execCommand(t, "go", "build", "-o", bin, "github.com/luckylhb90/bnbloc/cmd/bnbloc-vmexec"); err != nil {
		t.Fatalf("build the helper: %v\n%s", err, out)
	}
	userHome, _ := os.UserHomeDir()
	acpHome, err := os.MkdirTemp(userHome, ".bnbloc-acp-real-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(acpHome) })
	agent := AgentConfig{Name: "claude", Command: npx, Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}}
	h := newHarnessAt(t, Config{Agents: []AgentConfig{agent}}, acpHome)
	dir := t.TempDir()
	exe := sbExecution(dir, bin, "unused-token", "heal-space")
	exe.Sandbox.URL = "http://127.0.0.1:9"
	exe.Sandbox.HostConfine.DenyRead, exe.Sandbox.HostConfine.DenyWrite = nil, nil
	create := func() error {
		ws := h.workdirWorkspace(dir)
		_, err := h.client.CreateSession(context.Background(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "claude",
			Prompt: "Reply with exactly the word ok.", Options: core.SessionOptions{Execution: exe}})
		return err
	}
	for attempt := 0; create() != nil; attempt++ {
		if attempt >= 3 {
			t.Fatal("the first start never succeeded")
		}
	}
	matches, _ := filepath.Glob(filepath.Join(acpHome, "sandbox", "npm-claude-*", "_npx", "*", "node_modules", "@anthropic-ai", "claude-agent-sdk-darwin-arm64", "claude"))
	if len(matches) != 1 {
		t.Fatalf("the agent's platform binary: %v", matches)
	}
	whole, _ := os.Stat(matches[0])
	if err := os.Truncate(matches[0], whole.Size()*9/10); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for attempt := 0; ; attempt++ {
		err := create()
		if err == nil {
			break
		}
		failed++
		t.Logf("start on the damaged cache: %v", err)
		if attempt >= 3 {
			t.Fatalf("the cache never healed: %v", err)
		}
	}
	if healed, _ := os.Stat(matches[0]); healed == nil || healed.Size() != whole.Size() {
		t.Errorf("the platform binary was not replaced: %v (want %d bytes)", healed, whole.Size())
	}
	t.Logf("healed after %d failed start(s)", failed)
}
