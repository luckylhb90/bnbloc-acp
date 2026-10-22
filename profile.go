package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/luckylhb90/bnbloc/core"
)

// Agent profiles (Core Protocol capability agentProfile): the role a session
// wears — an expert's instructions, the skills it loads on demand, the MCP
// servers it may call and the env those need. The session-create body
// carries it as protocol.AgentProfile, validated by its Core() into a
// core.AgentProfile; the adapter maps that onto each agent per its
// agents.json `profile` block (ProfileConfig). The profile lives in memory
// with its session (never written to disk except the instructions/skill
// files it asks for; env and header values are secrets and never logged).

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ProfileConfig is an agent's mapping of a session profile (agents.json
// `profile`). Agents without a block use defaultProfileConfig.
type ProfileConfig struct {
	// InstructionsVia is how the profile's instructions reach the agent:
	// "meta" (ACP session/new _meta.systemPrompt.append), "env:<NAME>" (the
	// session's agent process env var NAME) or "file:<path>" (written into the
	// workspace at <path>, one of the agent-config paths, e.g. AGENTS.md).
	InstructionsVia string `json:"instructionsVia,omitempty"`
	// SkillsDir is the workspace-relative directory skills are written into
	// (<SkillsDir>/<name>/SKILL.md), inside the agent-config paths.
	SkillsDir string `json:"skillsDir,omitempty"`
	// MCP passes the profile's MCP servers to the agent (default true).
	MCP *bool `json:"mcp,omitempty"`
}

// defaultProfileConfig applies to an agent without a profile block:
// AGENTS.md (the cross-agent instructions file) and .agents/skills.
var defaultProfileConfig = ProfileConfig{InstructionsVia: "file:AGENTS.md", SkillsDir: ".agents/skills"}

// Instruction delivery forms.
const (
	viaMeta = "meta"
	viaEnv  = "env:"
	viaFile = "file:"
)

func (pc ProfileConfig) mcpEnabled() bool { return pc.MCP == nil || *pc.MCP }

func (pc ProfileConfig) validate() error {
	via := pc.InstructionsVia
	switch {
	case via == viaMeta:
	case strings.HasPrefix(via, viaEnv):
		name := strings.TrimPrefix(via, viaEnv)
		if !envNameRE.MatchString(name) || strings.HasPrefix(name, "BNBLOC_ACP_") {
			return fmt.Errorf("profile.instructionsVia %q: env:<NAME> with a POSIX name (not BNBLOC_ACP_*)", via)
		}
	case strings.HasPrefix(via, viaFile):
		p := strings.TrimPrefix(via, viaFile)
		if !agentConfigFile(p) {
			return fmt.Errorf("profile.instructionsVia %q: the file must be an agent-config path (e.g. AGENTS.md, CLAUDE.md)", via)
		}
	default:
		return fmt.Errorf("profile.instructionsVia %q: want meta, env:<NAME> or file:<path>", via)
	}
	if !agentConfigDir(pc.SkillsDir) {
		return fmt.Errorf("profile.skillsDir %q: must be a directory inside the agent-config paths (e.g. .claude/skills, .agents/skills, .opencode/skills)", pc.SkillsDir)
	}
	return nil
}

// effectiveProfileConfig is the agent's mapping, its block or the default.
func (a AgentConfig) effectiveProfileConfig() ProfileConfig {
	if a.Profile == nil {
		return defaultProfileConfig
	}
	pc := *a.Profile
	if pc.InstructionsVia == "" {
		pc.InstructionsVia = defaultProfileConfig.InstructionsVia
	}
	if pc.SkillsDir == "" {
		pc.SkillsDir = defaultProfileConfig.SkillsDir
	}
	return pc
}

// profileMu serializes profile materialization: sessions of one workspace
// (a new one, an earlier one continuing in a new process) share its
// agent-config files.
var profileMu sync.Mutex

// profileRecord is what the adapter wrote into a workspace for profiles, kept
// beside the workspace directory (<workspaces>/<id>.profile.json, outside
// the agent's reach and never a deliverable) so a later session, also after
// an adapter restart, removes exactly that and nothing of the agent's.
type profileRecord struct {
	// InstructionsFile is the instructions file written (file:<path>).
	InstructionsFile string `json:"instructionsFile,omitempty"`
	// SkillsDir is the skills directory the adapter owns while a profile is
	// worn: it holds exactly the profile's skills.
	SkillsDir string `json:"skillsDir,omitempty"`
}

func (ws workspaceRoot) profileRecordPath() string {
	if ws.recordBase != "" {
		return ws.recordBase + ".profile.json"
	}
	return ws.dir + ".profile.json"
}

// readProfileRecord returns the workspace's record; a missing or unreadable
// one is empty, and a path that is not an agent-config path is dropped (the
// record never widens what may be removed).
func (ws workspaceRoot) readProfileRecord() profileRecord {
	var rec profileRecord
	raw, err := os.ReadFile(ws.profileRecordPath())
	if err != nil {
		return rec
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		slog.Warn("bnbloc-acp ignoring an unreadable profile record", "path", ws.profileRecordPath(), "error", err)
		return profileRecord{}
	}
	if !agentConfigFile(rec.InstructionsFile) {
		rec.InstructionsFile = ""
	}
	if !agentConfigDir(rec.SkillsDir) {
		rec.SkillsDir = ""
	}
	return rec
}

func (ws workspaceRoot) writeProfileRecord(rec profileRecord) error {
	if rec == (profileRecord{}) {
		if err := os.Remove(ws.profileRecordPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(ws.profileRecordPath(), raw, 0o600)
}

// agentConfigFile / agentConfigDir: a workspace-relative agent-config file,
// or a directory whose contents are agent-config paths (never .room/).
func agentConfigFile(rel string) bool {
	return rel != "" && core.WorkspaceFileAllowed(rel) && !strings.HasPrefix(rel, core.RoomContextDir)
}

func agentConfigDir(rel string) bool {
	return rel != "" && agentConfigFile(rel+"/x/SKILL.md")
}

// removeConfigPath removes one agent-config file or directory of the
// workspace. The path's parent must resolve to itself inside the workspace:
// a symlinked parent could point at the agent's project files, so it is
// refused rather than followed; a symlink at the path itself is removed as a
// link (never its target).
func (ws workspaceRoot) removeConfigPath(rel string) error {
	lexical := filepath.Join(ws.real, filepath.FromSlash(rel))
	parent, err := ws.resolve(filepath.Dir(filepath.Join(ws.dir, filepath.FromSlash(rel))))
	if err != nil {
		return err
	}
	if parent != filepath.Dir(lexical) {
		return fmt.Errorf("%w: %s goes through a symlink", errOutsideWorkspace, rel)
	}
	return os.RemoveAll(lexical)
}

// materializeProfile makes the workspace wear the session's profile, and
// only it: the workspace outlives its sessions, so first what the adapter
// wrote for an earlier profile goes (its instructions file and the skills
// directory it owns, per the workspace's profileRecord, plus this agent's
// skills directory), then the instructions file (file:<path>) and every
// skill under the skills dir are written. The agent's own files are never
// touched: only agent-config paths are removed. A session without a profile
// removes the earlier profile's files and wears none. Run at session open
// and again when the session continues in a new agent process.
func materializeProfile(p *core.AgentProfile, ws workspaceRoot, pc ProfileConfig) error {
	profileMu.Lock()
	defer profileMu.Unlock()
	prev := ws.readProfileRecord()
	var next profileRecord
	if p != nil {
		next.SkillsDir = pc.SkillsDir
		if strings.HasPrefix(pc.InstructionsVia, viaFile) && strings.TrimSpace(p.Instructions) != "" {
			next.InstructionsFile = strings.TrimPrefix(pc.InstructionsVia, viaFile)
		}
	}
	// The record first names everything the workspace may hold afterwards, so
	// a failure half way never leaves a file the next session would miss.
	if err := ws.writeProfileRecord(profileRecord{
		InstructionsFile: firstNonEmpty(next.InstructionsFile, prev.InstructionsFile),
		SkillsDir:        firstNonEmpty(next.SkillsDir, prev.SkillsDir),
	}); err != nil {
		return err
	}
	if prev.InstructionsFile != "" && prev.InstructionsFile != next.InstructionsFile {
		if err := ws.removeConfigPath(prev.InstructionsFile); err != nil {
			return err
		}
	}
	for _, dir := range []string{prev.SkillsDir, next.SkillsDir} {
		if dir != "" {
			if err := ws.removeConfigPath(dir); err != nil {
				return err
			}
		}
	}
	if p != nil {
		if next.InstructionsFile != "" {
			if err := ws.writeConfigFile(next.InstructionsFile, []byte(p.Instructions)); err != nil {
				return err
			}
		}
		for _, s := range p.Skills {
			for _, f := range s.Files {
				if err := ws.writeConfigFile(pc.SkillsDir+"/"+s.Name+"/"+f.Path, f.Content); err != nil {
					return err
				}
			}
		}
	}
	return ws.writeProfileRecord(next)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// profileMeta is the session/new _meta for instructionsVia "meta" (nil otherwise):
// the system-prompt append ACP agents built on the Claude Agent SDK read.
func profileMeta(p *core.AgentProfile, pc ProfileConfig) json.RawMessage {
	if p == nil || pc.InstructionsVia != viaMeta || strings.TrimSpace(p.Instructions) == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"systemPrompt": map[string]any{"append": p.Instructions}})
	return raw
}

// profileEnviron is the extra agent process env of the session: the profile's env
// plus the instructions for instructionsVia "env:<NAME>". Each Core Protocol
// session runs in its own agent process, so this env is per session and
// applies at spawn (and again when the session continues in a new process).
func profileEnviron(p *core.AgentProfile, pc ProfileConfig) []string {
	if p == nil {
		return nil
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+p.Env[k])
	}
	if strings.HasPrefix(pc.InstructionsVia, viaEnv) && strings.TrimSpace(p.Instructions) != "" {
		out = append(out, strings.TrimPrefix(pc.InstructionsVia, viaEnv)+"="+p.Instructions)
	}
	return out
}

// acpMcpServer is one ACP McpServer: stdio carries name, command, args and
// env (arrays always present); http/sse carry type, name, url and headers.
type acpMcpServer struct {
	Type    string            `json:"type,omitempty"`
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    *[]string         `json:"args,omitempty"`
	Env     *[]acpEnvVariable `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers *[]acpEnvVariable `json:"headers,omitempty"`
}

func toACPServer(m core.MCPServer) acpMcpServer {
	nonNilEnv := func(in []core.NameValue) *[]acpEnvVariable {
		out := make([]acpEnvVariable, 0, len(in))
		for _, nv := range in {
			out = append(out, acpEnvVariable{Name: nv.Name, Value: nv.Value})
		}
		return &out
	}
	if m.Transport() == core.MCPStdio {
		args := m.Args
		if args == nil {
			args = []string{}
		}
		return acpMcpServer{Name: m.Name, Command: m.Command, Args: &args, Env: nonNilEnv(m.Env)}
	}
	return acpMcpServer{Type: m.Transport(), Name: m.Name, URL: m.URL, Headers: nonNilEnv(m.Headers)}
}

// errMcpClash: a profile MCP server has the name of one the agent's config
// already passes (bad_request: the session would not get the server its
// role requires).
var errMcpClash = errors.New("mcp server name clash")

// mcpDrop is a profile MCP server the session could not be given, and why
// (said to the agent and the room: mcpnote.go).
type mcpDrop struct {
	Name   string
	Reason string
}

// Reasons a profile MCP server is not passed.
const (
	dropMCPOff      = "this agent's bnbloc-acp config passes no session MCP servers"
	dropNoCommand   = "its command was not found on this machine"
	dropNoTransport = "this agent takes no %s MCP servers"
)

// profileMCPServers is the session's ACP mcpServers: the agent's configured
// list plus the profile's servers the agent can be given, and the profile's
// servers it cannot (drops). A server is dropped when the agent's profile
// mapping turns MCP off, when it is http/sse and the agent's initialize did
// not advertise mcpCapabilities.http / .sse, or when it is stdio and its
// command cannot be found the way the agent would start it (an absolute or
// workspace-relative path that is no executable file, a bare name in no
// PATH directory of the agent's or the server's env). Logs name the server,
// its command and the reason, never its env or header values. A profile server named like a
// configured one is refused (errMcpClash). dir is the session workspace,
// env the agent process environment.
func profileMCPServers(p *core.AgentProfile, a AgentConfig, init acpInitializeResponse, dir string, env []string) (json.RawMessage, []mcpDrop, error) {
	base := a.mcpServers()
	if p == nil || len(p.MCPServers) == 0 {
		return base, nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(base, &list); err != nil {
		return nil, nil, err
	}
	configured := map[string]bool{}
	for _, raw := range list {
		var n struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &n) == nil {
			configured[n.Name] = true
		}
	}
	caps := init.AgentCapabilities.McpCapabilities
	enabled := a.effectiveProfileConfig().mcpEnabled()
	var drops []mcpDrop
	for _, m := range p.MCPServers {
		if configured[m.Name] {
			return nil, nil, fmt.Errorf("%w: %q is configured for agent %s and also required by the session profile", errMcpClash, m.Name, a.Name)
		}
		reason := ""
		switch {
		case !enabled:
			reason = dropMCPOff
		case (m.Type == core.MCPHTTP && !caps.HTTP) || (m.Type == core.MCPSSE && !caps.SSE):
			reason = fmt.Sprintf(dropNoTransport, m.Type)
		case m.Transport() == core.MCPStdio && !commandFound(m.Command, dir, env, m.Env):
			reason = dropNoCommand
		}
		if reason != "" {
			slog.Warn("bnbloc-acp not passing an mcp server to the agent", "agent", a.Name, "server", m.Name, "command", m.Command, "reason", reason)
			drops = append(drops, mcpDrop{Name: m.Name, Reason: reason})
			continue
		}
		raw, err := json.Marshal(toACPServer(m))
		if err != nil {
			return nil, nil, err
		}
		list = append(list, raw)
	}
	out, err := json.Marshal(list)
	return out, drops, err
}

// commandFound reports whether a stdio MCP server's command resolves the
// way the agent starts it: a path (absolute, or relative to the workspace,
// the agent's cwd) must be an executable file; a bare name must be one in a
// PATH directory of the agent's environment or of the server's own env.
func commandFound(command, dir string, env []string, serverEnv []core.NameValue) bool {
	if command == "" {
		return false
	}
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
		if !filepath.IsAbs(command) {
			command = filepath.Join(dir, command)
		}
		return executable(command)
	}
	var paths []string
	if v, ok := envValue(env, "PATH"); ok {
		paths = append(paths, v)
	}
	for _, nv := range serverEnv {
		if envKeyEqual(nv.Name, "PATH") {
			paths = append(paths, nv.Value)
		}
	}
	for _, path := range paths {
		for _, d := range filepath.SplitList(path) {
			if d == "" {
				continue
			}
			if !filepath.IsAbs(d) {
				d = filepath.Join(dir, d)
			}
			if executable(filepath.Join(d, command)) {
				return true
			}
		}
	}
	return false
}

// envValue is the effective value of key in env: the last entry wins, as in
// os/exec.
func envValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && envKeyEqual(k, key) {
			return v, true
		}
	}
	return "", false
}

// writeConfigFile writes one agent-config file of the workspace (the
// profile's instructions file and skills; the bridge's PUT .../files for an
// agent-config path).
func (ws workspaceRoot) writeConfigFile(rel string, content []byte) error {
	if !agentConfigFile(rel) {
		return fmt.Errorf("%w: %s is not an agent-config path", errBadPath, rel)
	}
	return ws.writeAllowed(rel, content)
}
