package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Agent profiles (Core Protocol capability agentProfile): the role a session
// wears — an expert's instructions, the skills it loads on demand, the MCP
// servers it may call and the env those need. The adapter maps a profile
// onto each agent per its agents.json `profile` block (ProfileConfig).
//
// The wire types below mirror bnbloc's core/protocol AgentProfile exactly
// (same JSON tags) because the pinned bnbloc dependency predates them;
// switch to protocol.AgentProfile / CreateSessionRequest.Profile on the next
// dependency bump and delete these copies.

// capabilityAgentProfile mirrors protocol.CapabilityAgentProfile.
const capabilityAgentProfile = "agentProfile"

// wireProfile mirrors protocol.AgentProfile.
type wireProfile struct {
	Instructions string            `json:"instructions,omitempty"`
	Skills       []wireSkill       `json:"skills,omitempty"`
	McpServers   []wireMcpServer   `json:"mcpServers,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// wireSkill mirrors protocol.ProfileSkill.
type wireSkill struct {
	Name  string     `json:"name"`
	Files []wireFile `json:"files"`
}

// wireFile mirrors protocol.ProfileFile.
type wireFile struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
}

// wireMcpServer mirrors protocol.McpServer (the ACP McpServer union).
type wireMcpServer struct {
	Type    string           `json:"type,omitempty"`
	Name    string           `json:"name"`
	Command string           `json:"command,omitempty"`
	Args    []string         `json:"args,omitempty"`
	Env     []acpEnvVariable `json:"env,omitempty"`
	URL     string           `json:"url,omitempty"`
	Headers []acpEnvVariable `json:"headers,omitempty"`
}

// profileBody is the part of a session-create body the pinned
// protocol.CreateSessionRequest does not know yet.
type profileBody struct {
	Profile *wireProfile `json:"profile"`
}

// Bounds mirroring protocol.MaxProfileSkills / MaxProfileMcpServers.
const (
	maxProfileSkills  = 64
	maxProfileServers = 32
	maxBundlePath     = 200
)

// MCP transports.
const (
	mcpStdio = "stdio"
	mcpHTTP  = "http"
	mcpSSE   = "sse"
)

// profile is a validated session profile, kept in memory with its session
// (never written to disk except the instructions/skill files it asks for;
// env and header values are secrets and never logged).
type profile struct {
	instructions string
	skills       []profileSkill
	servers      []wireMcpServer // type normalized ("stdio" explicit)
	env          map[string]string
}

type profileSkill struct {
	name  string
	files []profileFile
}

type profileFile struct {
	path    string
	content []byte
}

var (
	bundleSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	envNameRE       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// validBundlePath mirrors bnbloc's core.ValidBundlePath.
func validBundlePath(p string) bool {
	if p == "" || len(p) > maxBundlePath {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || !bundleSegmentRE.MatchString(seg) {
			return false
		}
	}
	return true
}

// errBadProfile marks a profile the adapter refuses (bad_request).
var errBadProfile = errors.New("invalid profile")

func badProfile(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errBadProfile}, args...)...)
}

// decodeProfile reads and validates the optional profile of a session-create
// body, the same rules bnbloc's protocol.AgentProfile.Core applies.
func decodeProfile(raw []byte) (*profile, error) {
	var body profileBody
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, badProfile("%v", err)
	}
	w := body.Profile
	if w == nil {
		return nil, nil
	}
	if len(w.Skills) > maxProfileSkills {
		return nil, badProfile("more than %d skills", maxProfileSkills)
	}
	if len(w.McpServers) > maxProfileServers {
		return nil, badProfile("more than %d mcp servers", maxProfileServers)
	}
	p := &profile{instructions: w.Instructions}
	for k, v := range w.Env {
		if !envNameRE.MatchString(k) {
			return nil, badProfile("invalid env name %q", k)
		}
		if p.env == nil {
			p.env = map[string]string{}
		}
		p.env[k] = v
	}
	seenSkill := map[string]bool{}
	for _, s := range w.Skills {
		if !validBundlePath(s.Name) || strings.Contains(s.Name, "/") || strings.HasPrefix(s.Name, ".") {
			return nil, badProfile("invalid skill name %q", s.Name)
		}
		if seenSkill[s.Name] {
			return nil, badProfile("duplicate skill %q", s.Name)
		}
		seenSkill[s.Name] = true
		ps := profileSkill{name: s.Name}
		seenFile := map[string]bool{}
		for _, f := range s.Files {
			if !validBundlePath(f.Path) {
				return nil, badProfile("skill %s: invalid file path %q", s.Name, f.Path)
			}
			if seenFile[f.Path] {
				return nil, badProfile("skill %s: duplicate file %q", s.Name, f.Path)
			}
			seenFile[f.Path] = true
			content := []byte(f.Content)
			if f.ContentBase64 != "" {
				b, err := base64.StdEncoding.DecodeString(f.ContentBase64)
				if err != nil {
					return nil, badProfile("skill %s: file %q: invalid contentBase64", s.Name, f.Path)
				}
				content = b
			}
			ps.files = append(ps.files, profileFile{path: f.Path, content: content})
		}
		p.skills = append(p.skills, ps)
	}
	seenServer := map[string]bool{}
	for _, m := range w.McpServers {
		if strings.TrimSpace(m.Name) == "" {
			return nil, badProfile("mcp server without a name")
		}
		if seenServer[m.Name] {
			return nil, badProfile("duplicate mcp server %q", m.Name)
		}
		seenServer[m.Name] = true
		switch m.Type {
		case "", mcpStdio:
			if strings.TrimSpace(m.Command) == "" {
				return nil, badProfile("mcp server %q: stdio needs a command", m.Name)
			}
			m.Type = mcpStdio
		case mcpHTTP, mcpSSE:
			if strings.TrimSpace(m.URL) == "" {
				return nil, badProfile("mcp server %q: %s needs a url", m.Name, m.Type)
			}
		default:
			return nil, badProfile("mcp server %q: unknown type %q", m.Name, m.Type)
		}
		p.servers = append(p.servers, m)
	}
	return p, nil
}

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

// agentConfigPaths mirrors bnbloc's core.AgentConfigPaths: the only paths
// outside .room/ a workspace write accepts, read-only to the agent, never
// listed as deliverables. A trailing "/" names a directory tree.
var agentConfigPaths = []string{"CLAUDE.md", "AGENTS.md", ".claude/skills/", ".agents/skills/", ".opencode/", ".mcp.json"}

// isAgentConfigPath reports whether a clean workspace-relative slash path is,
// or lies under, an agent-config path.
func isAgentConfigPath(rel string) bool {
	for _, p := range agentConfigPaths {
		if strings.HasSuffix(p, "/") {
			if rel == strings.TrimSuffix(p, "/") || strings.HasPrefix(rel, p) {
				return true
			}
		} else if rel == p {
			return true
		}
	}
	return false
}

// workspaceFileAllowed mirrors bnbloc's core.WorkspaceFileAllowed: a clean
// relative file path inside .room/ or one of the agent-config paths.
func workspaceFileAllowed(rel string) bool {
	if rel == "" || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "/") || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." {
		return false
	}
	if strings.HasPrefix(rel, roomDir+"/") {
		return true
	}
	for _, p := range agentConfigPaths {
		if rel == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			return true
		}
	}
	return false
}

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
		if !workspaceFileAllowed(p) || strings.HasPrefix(p, roomDir+"/") {
			return fmt.Errorf("profile.instructionsVia %q: the file must be an agent-config path (e.g. AGENTS.md, CLAUDE.md)", via)
		}
	default:
		return fmt.Errorf("profile.instructionsVia %q: want meta, env:<NAME> or file:<path>", via)
	}
	if !workspaceFileAllowed(pc.SkillsDir+"/x/SKILL.md") || strings.HasPrefix(pc.SkillsDir, roomDir) {
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

// materialize writes the profile's files into the workspace: the
// instructions file (file:<path>) and every skill under the skills dir (a
// skill's directory is replaced, so a re-materialization leaves no stale
// files). Run at session open and again when the session continues in a new
// agent process.
func (p *profile) materialize(ws workspaceRoot, pc ProfileConfig) error {
	if p == nil {
		return nil
	}
	if strings.HasPrefix(pc.InstructionsVia, viaFile) && strings.TrimSpace(p.instructions) != "" {
		if err := ws.writeConfigFile(strings.TrimPrefix(pc.InstructionsVia, viaFile), []byte(p.instructions)); err != nil {
			return err
		}
	}
	for _, s := range p.skills {
		dir := pc.SkillsDir + "/" + s.name
		real, err := ws.resolve(filepath.Join(ws.dir, filepath.FromSlash(dir)))
		if err != nil {
			return err
		}
		if err := os.RemoveAll(real); err != nil {
			return err
		}
		for _, f := range s.files {
			if err := ws.writeConfigFile(dir+"/"+f.path, f.content); err != nil {
				return err
			}
		}
	}
	return nil
}

// meta is the session/new _meta for instructionsVia "meta" (nil otherwise):
// the system-prompt append ACP agents built on the Claude Agent SDK read.
func (p *profile) meta(pc ProfileConfig) json.RawMessage {
	if p == nil || pc.InstructionsVia != viaMeta || strings.TrimSpace(p.instructions) == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"systemPrompt": map[string]any{"append": p.instructions}})
	return raw
}

// environ is the extra agent process env of the session: the profile's env
// plus the instructions for instructionsVia "env:<NAME>". Each Core Protocol
// session runs in its own agent process, so this env is per session and
// applies at spawn (and again when the session continues in a new process).
func (p *profile) environ(pc ProfileConfig) []string {
	if p == nil {
		return nil
	}
	keys := make([]string, 0, len(p.env))
	for k := range p.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+p.env[k])
	}
	if strings.HasPrefix(pc.InstructionsVia, viaEnv) && strings.TrimSpace(p.instructions) != "" {
		out = append(out, strings.TrimPrefix(pc.InstructionsVia, viaEnv)+"="+p.instructions)
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

func toACPServer(m wireMcpServer) acpMcpServer {
	nonNilEnv := func(v []acpEnvVariable) *[]acpEnvVariable {
		if v == nil {
			v = []acpEnvVariable{}
		}
		return &v
	}
	if m.Type == mcpStdio {
		args := m.Args
		if args == nil {
			args = []string{}
		}
		return acpMcpServer{Name: m.Name, Command: m.Command, Args: &args, Env: nonNilEnv(m.Env)}
	}
	return acpMcpServer{Type: m.Type, Name: m.Name, URL: m.URL, Headers: nonNilEnv(m.Headers)}
}

// errMcpClash: a profile MCP server has the name of one the agent's config
// already passes (bad_request: the session would not get the server its
// role requires).
var errMcpClash = errors.New("mcp server name clash")

// mcpServers is the session's ACP mcpServers: the agent's configured list
// plus, when its profile mapping allows MCP, the profile's servers the agent
// supports (an http/sse server goes only to an agent whose initialize
// advertised mcpCapabilities.http / .sse; others are dropped with a warning
// naming the server, never its headers). A profile server named like a
// configured one is refused (errMcpClash).
func (p *profile) mcpServers(a AgentConfig, init acpInitializeResponse) (json.RawMessage, error) {
	base := a.mcpServers()
	if p == nil || len(p.servers) == 0 || !a.effectiveProfileConfig().mcpEnabled() {
		return base, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(base, &list); err != nil {
		return nil, err
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
	for _, m := range p.servers {
		if configured[m.Name] {
			return nil, fmt.Errorf("%w: %q is configured for agent %s and also required by the session profile", errMcpClash, m.Name, a.Name)
		}
		if (m.Type == mcpHTTP && !caps.HTTP) || (m.Type == mcpSSE && !caps.SSE) {
			slog.Warn("bnbloc-acp dropping an mcp server the agent does not support", "agent", a.Name, "server", m.Name, "type", m.Type)
			continue
		}
		raw, err := json.Marshal(toACPServer(m))
		if err != nil {
			return nil, err
		}
		list = append(list, raw)
	}
	return json.Marshal(list)
}

// writeConfigFile writes one agent-config file of the workspace (the
// profile's instructions file and skills; the bridge's PUT .../files for an
// agent-config path).
func (ws workspaceRoot) writeConfigFile(rel string, content []byte) error {
	if !workspaceFileAllowed(rel) || strings.HasPrefix(rel, roomDir+"/") {
		return fmt.Errorf("%w: %s is not an agent-config path", errBadPath, rel)
	}
	return ws.writeAllowed(rel, content)
}
