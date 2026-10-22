package main

// Sandbox execution (Core Protocol capability sandboxExec,
// docs/prd/2026-10-22-acp-sandbox.md in the bnbloc repository).
//
// The agent CLI keeps running on the host, confined by the OS sandbox through
// `bnbloc-vmexec confine`, while the shell commands it runs are routed into
// the room's sandbox space. The confinement covers the agent process and
// everything it spawns, whatever way it runs a command; the routing is a seam
// each CLI has its own way of (opencode: a tool.execute.before plugin that
// hands the bash tool's command to `vmexec hook opencode`). An agent whose CLI
// has no routing this core implements cannot run sandboxed: a sandboxed
// request for it is refused, and nothing runs on the host.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// sandboxRoutingOpencode is the one routing implemented: opencode's plugin hook.
const sandboxRoutingOpencode = "opencode"

// validSandboxRouting reports a value of an agent's `sandbox` setting.
func validSandboxRouting(s string) bool { return s == "" || s == sandboxRoutingOpencode }

// sandboxHere reports whether this machine can confine an agent process
// (vmexec confine: Seatbelt on macOS, bubblewrap on Linux).
func sandboxHere() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

// serverCapabilities are the optional Core Protocol capabilities served:
// agentProfile is mapped per agent (profile.go), sandboxExec is advertised
// where the helper can confine and answered per agent (an agent without a
// routing is refused).
func serverCapabilities() []string {
	caps := []string{
		protocol.CapabilityInterrupt, protocol.CapabilityWriteFile, protocol.CapabilityDeleteWorkspace,
		protocol.CapabilitySessionFiles, protocol.CapabilitySessionFileBytes, protocol.CapabilityAgentProfile,
		capabilityTurnActivity,
	}
	if sandboxHere() {
		caps = append(caps, protocol.CapabilitySandboxExec)
	}
	return caps
}

// sandboxUnsupported is why an agent cannot run sandboxed ("" = it can).
func (a AgentConfig) sandboxUnsupported() string {
	switch {
	case !sandboxHere():
		return "the agent sandbox confines agents on macOS and Linux only"
	case a.Sandbox == "":
		return "its config has no `sandbox` routing for this agent's CLI (supported: " + sandboxRoutingOpencode + ")"
	}
	return ""
}

// sandboxStatus is the agent's sandbox routing, or why it has none.
func (a AgentConfig) sandboxStatus() (routing, note string) {
	if why := a.sandboxUnsupported(); why != "" {
		return "", why
	}
	return a.Sandbox, ""
}

// harnesses are the vmexec confine profiles of a routing: the CLI's own, then
// those whose state it reads and writes (opencode reads Claude Code's skills
// and CLAUDE.md natively).
func sandboxHarnesses(routing string) []string {
	if routing == sandboxRoutingOpencode {
		return []string{"opencode", "claude"}
	}
	return []string{"generic"}
}

// errSandboxRefused marks a sandboxed request the core refuses (a bad request:
// the agent, the workspace or the descriptor cannot be honoured).
var errSandboxRefused = errors.New("cannot run sandboxed")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSandboxRefused, fmt.Sprintf(format, args...))
}

// sandboxHost is what the adapter keeps for sandboxed sessions: a state
// directory of its own (token files, the routing's config) and its home,
// which a confined agent may not read beyond its own files (it holds the
// agents' config, which can carry keys, and every other workspace).
type sandboxHost struct {
	stateDir string
	home     string
}

// checkExecution validates a request's sandbox descriptor against the agent and
// the workspace, and returns the descriptor (nil: a host session). A workspace
// made in a sandbox workdir takes only sandboxed sessions, and a sandboxed
// session needs such a workspace, at the descriptor's workdir.
func checkExecution(agent AgentConfig, ws workspaceRoot, exe *protocol.Execution) (*core.SandboxExecution, error) {
	if exe == nil {
		if ws.workdirBacked {
			return nil, refuse("this workspace was made in the sandbox workdir and takes only sandboxed sessions")
		}
		return nil, nil
	}
	ce, err := exe.Core()
	if err != nil {
		return nil, err
	}
	s := ce.Sandbox
	if why := agent.sandboxUnsupported(); why != "" {
		return nil, refuse("the agent %s: %s", agent.Name, why)
	}
	if !ws.workdirBacked {
		return nil, refuse("a sandboxed session needs a workspace created in the sandbox workdir")
	}
	if !filepath.IsAbs(s.Workdir) || !sameDir(ws.real, s.Workdir) {
		return nil, refuse("the descriptor's workdir %q is not this workspace", s.Workdir)
	}
	if s.HostConfine.Workspace != "" && !sameDir(ws.real, s.HostConfine.Workspace) {
		return nil, refuse("the host confinement's workspace %q is not this workspace", s.HostConfine.Workspace)
	}
	if st, err := os.Stat(s.VMExec); !filepath.IsAbs(s.VMExec) || err != nil || !st.Mode().IsRegular() {
		return nil, refuse("the sandbox helper %q is not an absolute path to a file", s.VMExec)
	}
	return &s, nil
}

// sameDir reports whether two directories are the same once symlinks resolve.
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// execPin is what a process spawn depends on: the descriptor but for the exec
// token and its expiry, which are renewed in place (the token file). A new
// input with another pin continues the session in a new process.
func execPin(s core.SandboxExecution) string {
	s.ExecToken, s.ExpiresAtMs = "", 0
	raw, _ := json.Marshal(s)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// sandboxLaunch is how one agent process is started confined.
type sandboxLaunch struct {
	vmexec string
	flags  []string // vmexec confine flags
	env    []string // the descriptor's and the routing's environment, PWD
	dir    string   // the workdir
}

// command is `<vmexec> confine <flags> -- <agent path> <args>`, in the workdir:
// confine replaces itself with the agent (exec), so stopping the process stops
// the agent.
func (l *sandboxLaunch) command(path string, args []string) *exec.Cmd {
	argv := append([]string{"confine"}, l.flags...)
	argv = append(argv, "--", path)
	argv = append(argv, args...)
	cmd := exec.Command(l.vmexec, argv...)
	cmd.Dir = l.dir
	return cmd
}

// tokenDir is the directory of one session's exec token file.
func (h sandboxHost) tokenDir(sessionID string) string {
	return filepath.Join(h.stateDir, "sessions", sessionID)
}

// writeToken writes the session's current exec token (0600, atomically) where
// vmexec reads it at every command. The agent may read the directory and never
// write it.
func (h sandboxHost) writeToken(sessionID, token string) (string, error) {
	dir := h.tokenDir(sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "token")
	return path, writeFileAtomic(path, []byte(token), 0o600)
}

func (h sandboxHost) removeToken(sessionID string) { _ = os.RemoveAll(h.tokenDir(sessionID)) }

// prepareLaunch builds the confined launch of an agent for the session's
// current descriptor: the token file, the routing's config, the confine flags
// and the environment. It fails — and the request is refused — when the
// routing cannot be set up: a sandboxed session never falls back to the host.
func (h sandboxHost) prepareLaunch(ctx context.Context, agent AgentConfig, s core.SandboxExecution, sessionID string) (*sandboxLaunch, error) {
	if why := agent.sandboxUnsupported(); why != "" {
		return nil, refuse("the agent %s: %s", agent.Name, why)
	}
	tokenFile, err := h.writeToken(sessionID, s.ExecToken)
	if err != nil {
		return nil, fmt.Errorf("the exec token file: %w", err)
	}
	tokenDir := filepath.Dir(tokenFile)
	// The descriptor without its token: the token is the file's, never the
	// environment's, so a stale one is never held by a process that outlives it.
	envSB := s
	envSB.ExecToken = ""
	hc := s.HostConfine
	env := protocol.VMExecEnv(protocol.SandboxExecution{
		URL: envSB.URL, ExpiresAtMs: 0, Machine: envSB.Machine, Space: envSB.Space, VMExec: envSB.VMExec,
		Workdir: envSB.Workdir, Mounts: wireMounts(envSB.Mounts), Passthrough: envSB.Passthrough, Env: envSB.Env,
		HostConfine: &protocol.HostConfine{Workspace: firstNonEmpty(hc.Workspace, s.Workdir), AllowRead: hc.AllowRead, AllowWrite: hc.AllowWrite, DenyWrite: hc.DenyWrite, DenyRead: hc.DenyRead},
	})
	env = append(env, protocol.EnvVMExecTokenFile+"="+tokenFile, "PWD="+s.Workdir)

	l := &sandboxLaunch{vmexec: s.VMExec, dir: s.Workdir}
	for _, hn := range sandboxHarnesses(agent.Sandbox) {
		l.flags = append(l.flags, "--harness", hn)
	}
	// The adapter's home holds the agents' config (it can carry keys) and every
	// other workspace: a confined agent reads none of it. Its own token
	// directory is re-allowed (a sub-path allow wins over the ancestor's deny).
	l.flags = append(l.flags, "--deny-read", h.home, "--allow-read", tokenDir, "--deny-write", tokenDir)

	switch agent.Sandbox {
	case sandboxRoutingOpencode:
		cfgDir, err := h.prepareOpencode(ctx, l, env, s)
		if err != nil {
			return nil, err
		}
		env = append(env, "OPENCODE_CONFIG_DIR="+cfgDir)
		l.flags = append(l.flags, "--allow-read", cfgDir, "--deny-write", cfgDir)
	}
	l.env = env
	return l, nil
}

func wireMounts(ms []core.PathMount) []protocol.PathMount {
	out := make([]protocol.PathMount, 0, len(ms))
	for _, m := range ms {
		out = append(out, protocol.PathMount{Host: m.Host, Guest: m.Guest})
	}
	return out
}

// opencodePlugin is the tool.execute.before plugin loaded through
// OPENCODE_CONFIG_DIR. The rewrite itself (and the host passthrough
// allowlist) lives in vmexec only: the plugin hands {tool,args} to
// `vmexec hook opencode` and applies the args it returns; a failure blocks the
// tool call rather than letting it run on the host.
const opencodePlugin = `// Generated by bnbloc-acp for sandboxed sessions. Do not edit.
const { spawnSync } = require("child_process");
const VMEXEC = %s;
export const BnblocVMExec = async () => ({
  "tool.execute.before": async (input, output) => {
    if (input.tool !== "bash" || !output || !output.args) return;
    const r = spawnSync(VMEXEC, ["hook", "opencode"], {
      input: JSON.stringify({ tool: input.tool, args: output.args }),
      encoding: "utf8",
      env: process.env,
    });
    if (r.error || r.status !== 0) {
      throw new Error("bnbloc sandbox: command refused: " + ((r.stderr || "").trim() || (r.error ? r.error.message : "vmexec exit " + r.status)));
    }
    const out = JSON.parse(r.stdout);
    if (!out || typeof out.args !== "object") throw new Error("bnbloc sandbox: vmexec hook returned no args");
    for (const k of Object.keys(output.args)) delete output.args[k];
    Object.assign(output.args, out.args);
  },
});
`

// warmOpencodeFn is warmOpencode (a variable for tests: the real one runs the
// installed opencode).
var warmOpencodeFn = warmOpencode

// prepareOpencode makes the routing config directory (the plugin, a setup
// opencode can start in without writing) and returns it. The directory is
// write-denied to the agent: a writable one would let it plant a plugin that
// undoes the routing.
func (h sandboxHost) prepareOpencode(ctx context.Context, l *sandboxLaunch, env []string, s core.SandboxExecution) (string, error) {
	sum := sha256.Sum256([]byte(s.VMExec))
	dir := filepath.Join(h.stateDir, "opencode-"+hex.EncodeToString(sum[:4]))
	pluginDir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return "", fmt.Errorf("opencode routing config: %w", err)
	}
	vm, _ := json.Marshal(s.VMExec)
	if err := writeFileAtomic(filepath.Join(pluginDir, "bnbloc-vmexec.js"), []byte(fmt.Sprintf(opencodePlugin, vm)), 0o644); err != nil {
		return "", fmt.Errorf("opencode routing plugin: %w", err)
	}
	if err := warmOpencodeFn(ctx, l, env, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// warmOpencode sets dir up for the installed opencode: opencode writes
// .gitignore and package.json (pinning its plugin SDK to its own version) into
// every config directory and installs that SDK; in a write-denied one it
// aborts instead. A confined `opencode models` (no agent turn, writing only
// there) does it, once per opencode version; its dependency install goes
// through npm, so it uses a private npm cache inside the directory.
//
// The setup is ready within seconds, but `opencode models` may go on (it asks
// the network for the model list): the directory is polled and the command
// ended as soon as it is ready. The preparation is one at a time per directory
// and does not belong to the request that asked for it: a request that gives
// up (the bridge's client timed out) leaves it running, and the retry finds the
// directory ready.
func warmOpencode(ctx context.Context, l *sandboxLaunch, env []string, dir string) error {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		return refuse("opencode was not found on PATH: %v", err)
	}
	ver, err := opencodeVersion(ctx, bin)
	if err != nil {
		return err
	}
	if opencodeConfigWarm(dir, ver) {
		return nil
	}
	return warmFlight(ctx, dir, func(run context.Context) error { return runOpencodeWarm(run, l, env, dir, bin, ver) })
}

// warmFlights are the preparations in progress, by directory.
var (
	warmMu      sync.Mutex
	warmFlights = map[string]*flight{}
)

type flight struct {
	done chan struct{}
	err  error
}

// warmFlight runs do once at a time for key, in its own context, and waits for
// it as long as ctx allows.
func warmFlight(ctx context.Context, key string, do func(context.Context) error) error {
	warmMu.Lock()
	f := warmFlights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		warmFlights[key] = f
		go func() {
			run, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			f.err = do(run)
			warmMu.Lock()
			delete(warmFlights, key)
			warmMu.Unlock()
			close(f.done)
		}()
	}
	warmMu.Unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return fmt.Errorf("the opencode routing config is still being prepared (try again shortly): %w", ctx.Err())
	}
}

func runOpencodeWarm(ctx context.Context, l *sandboxLaunch, env []string, dir, bin, ver string) error {
	argv := []string{"confine"}
	for _, hn := range sandboxHarnesses(sandboxRoutingOpencode) {
		argv = append(argv, "--harness", hn)
	}
	argv = append(argv, "--allow-write", dir, "--", bin, "models")
	cmd := exec.Command(l.vmexec, argv...)
	cmd.Dir = l.dir
	// The adapter's own environment (HOME, PATH: confine and npm need them),
	// minus its settings, then the descriptor's and the routing's.
	cmd.Env = append(append(adapterEnviron(), env...), "OPENCODE_CONFIG_DIR="+dir, "npm_config_cache="+filepath.Join(dir, ".npm-cache"))
	ownProcessGroup(cmd)
	out := &outputTail{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("prepare the opencode routing config: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() {
		killTree(cmd, true)
		<-exited
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			if opencodeConfigWarm(dir, ver) {
				return nil
			}
			return fmt.Errorf("prepare the opencode routing config (opencode models): %v: %s", err, tailText(out.String(), 400))
		case <-tick.C:
			if opencodeConfigWarm(dir, ver) {
				stop()
				return nil
			}
		case <-ctx.Done():
			stop()
			if opencodeConfigWarm(dir, ver) {
				return nil
			}
			return fmt.Errorf("opencode %s did not set up its routing config directory %s in time: %s", ver, dir, tailText(out.String(), 400))
		}
	}
}

// adapterEnviron is the adapter's environment without its own BNBLOC_ACP_*
// settings (a token must never reach a process it starts), the base of every
// process it starts (AgentConfig.environ is the agent's, with its env on top).
func adapterEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BNBLOC_ACP_") {
			out = append(out, kv)
		}
	}
	return out
}

// opencodeVersions caches `opencode --version` per binary identity (path,
// size, mtime), so an upgrade is noticed without paying for it every session.
var opencodeVersions sync.Map

func opencodeVersion(ctx context.Context, bin string) (string, error) {
	fi, err := os.Stat(bin)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", bin, fi.Size(), fi.ModTime().UnixNano())
	if v, ok := opencodeVersions.Load(key); ok {
		return v.(string), nil
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("opencode --version: %w", err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", errors.New("opencode --version printed nothing")
	}
	opencodeVersions.Store(key, v)
	return v, nil
}

// opencodeConfigWarm reports whether opencode version ver would start in dir
// without writing: its .gitignore exists, package.json pins the plugin SDK to
// ver, and that version is installed.
func opencodeConfigWarm(dir, ver string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err != nil {
		return false
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil || json.Unmarshal(raw, &pkg) != nil || pkg.Dependencies["@opencode-ai/plugin"] != ver {
		return false
	}
	var installed struct {
		Version string `json:"version"`
	}
	raw, err = os.ReadFile(filepath.Join(dir, "node_modules", "@opencode-ai", "plugin", "package.json"))
	return err == nil && json.Unmarshal(raw, &installed) == nil && installed.Version == ver
}

// outputTail keeps the last max bytes written to it: a process's output, for
// a failure's reason.
type outputTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *outputTail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *outputTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// writeFileAtomic writes a file beside its final name and renames it over it.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
