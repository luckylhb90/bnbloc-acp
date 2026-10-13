package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// sessionWithProfile creates a session over the raw wire (so a test can send
// shapes the typed client would never build) and returns its id, or the
// error envelope.
func (h *harness) sessionWithProfile(ws, target, prompt string, prof map[string]any) (string, *protocol.ProtocolError) {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]any{"targetId": target, "prompt": prompt, "profile": prof})
	resp, err := h.srv.Client().Post(h.srv.URL+protocol.WorkspaceSessionsPath(ws), "application/json", strings.NewReader(string(raw)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var env protocol.ErrorEnvelope
		_ = json.Unmarshal(body, &env)
		return "", &env.Error
	}
	var out protocol.CreateSessionResponse
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatal(err)
	}
	return out.SessionID, nil
}

func sampleWireProfile() map[string]any {
	return map[string]any{
		"instructions": "Always end with MARKER-ALPHA.",
		"skills": []map[string]any{{"name": "marker-skill", "files": []map[string]any{
			{"path": "SKILL.md", "content": "---\nname: marker-skill\ndescription: Use for the secret phrase.\n---\nThe secret phrase is MARKER-BETA.\n"},
			{"path": "assets/blob.bin", "contentBase64": "AAEC"},
		}}},
		"mcpServers": []map[string]any{
			{"name": "tools", "command": "mcp-tools", "args": []string{"--x"}},
			{"type": "http", "name": "room", "url": "http://127.0.0.1:9/mcp", "headers": []map[string]any{{"name": "Authorization", "value": "Bearer secret-hdr"}}},
		},
		"env": map[string]string{"PROFILE_TOKEN": "tok-1"},
	}
}

func TestProfileIsAdvertised(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	if !core.Supports(h.ctx(), h.client, h.ep, core.CapabilityAgentProfile) {
		t.Fatal("agentProfile not advertised")
	}
}

func TestProfileConfigValidates(t *testing.T) {
	cases := map[string]string{
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"stdin"}}]}`:                        "instructionsVia",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"env:BAD-NAME"}}]}`:                 "env:<NAME>",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"env:BNBLOC_ACP_TOKEN"}}]}`:         "env:<NAME>",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"file:README.md"}}]}`:               "agent-config path",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"file:.room/x.md"}}]}`:              "agent-config path",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","skillsDir":"skills"}}]}`:    "skillsDir",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","skillsDir":"../x"}}]}`:      "skillsDir",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","skillsDir":".room/s"}}]}`:   "skillsDir",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","skillsDir":"AGENTS.md"}}]}`: "skillsDir",
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","unknown":1}}]}`:             "unknown field",
	}
	for raw, want := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseConfig(%s) = %v, want %q", raw, err, want)
		}
	}
	for _, raw := range []string{
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"meta","skillsDir":".claude/skills","mcp":false}}]}`,
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"file:AGENTS.md","skillsDir":".opencode/skills"}}]}`,
		`{"agents":[{"name":"a","command":"x","profile":{"instructionsVia":"env:AGENT_PROMPT"}}]}`,
		`{"agents":[{"name":"a","command":"x","profile":{}}]}`,
	} {
		if _, err := ParseConfig([]byte(raw)); err != nil {
			t.Errorf("ParseConfig(%s) = %v", raw, err)
		}
	}
}

// A profile protocol.AgentProfile.Core refuses is a bad_request, before any
// agent starts.
func TestCreateSessionRefusesUnsafeProfiles(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	for name, prof := range map[string]string{
		"escaping path":     `{"skills":[{"name":"a","files":[{"path":"../x"}]}]}`,
		"absolute path":     `{"skills":[{"name":"a","files":[{"path":"/etc/x"}]}]}`,
		"nested skill":      `{"skills":[{"name":"a/b","files":[]}]}`,
		"hidden skill":      `{"skills":[{"name":".x","files":[]}]}`,
		"duplicate skill":   `{"skills":[{"name":"a","files":[]},{"name":"a","files":[]}]}`,
		"duplicate file":    `{"skills":[{"name":"a","files":[{"path":"x"},{"path":"x"}]}]}`,
		"bad base64":        `{"skills":[{"name":"a","files":[{"path":"x","contentBase64":"!!"}]}]}`,
		"unnamed server":    `{"mcpServers":[{"command":"x"}]}`,
		"stdio no command":  `{"mcpServers":[{"name":"x"}]}`,
		"http no url":       `{"mcpServers":[{"name":"x","type":"http"}]}`,
		"unknown type":      `{"mcpServers":[{"name":"x","type":"ws","url":"ws://x"}]}`,
		"duplicate server":  `{"mcpServers":[{"name":"x","command":"a"},{"name":"x","command":"b"}]}`,
		"invalid env name":  `{"env":{"A-B":"1"}}`,
		"leading digit env": `{"env":{"1A":"1"}}`,
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(prof), &m); err != nil {
			t.Fatal(err)
		}
		if _, perr := h.sessionWithProfile(ws, "fake", "p", m); perr == nil || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "profile:") {
			t.Errorf("%s: %+v", name, perr)
		}
	}
	if _, perr := h.sessionWithProfile(ws, "fake", "p", nil); perr != nil {
		t.Fatalf("no profile: %+v", perr)
	}
}

// file: instructions + skills are written into the workspace; env reaches
// the session's agent process; stdio servers are passed and an http server
// is dropped for an agent without mcpCapabilities.http.
func TestProfileFileInstructionsSkillsEnvAndMcp(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", nil)
	agent.Profile = &ProfileConfig{InstructionsVia: "file:AGENTS.md", SkillsDir: ".opencode/skills"}
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, root := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "profile PROFILE_TOKEN", sampleWireProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	got := h.lastText(ws, sid)
	if !strings.Contains(got, "PROFILE_TOKEN=tok-1") || !strings.Contains(got, "meta= ") {
		t.Fatalf("reply = %s", got)
	}
	if !strings.Contains(got, `"name":"tools","command":"mcp-tools","args":["--x"],"env":[]`) || strings.Contains(got, `"room"`) {
		t.Fatalf("mcp servers = %s (stdio passed with arrays, http dropped)", got)
	}
	if raw, err := os.ReadFile(filepath.Join(root.dir, "AGENTS.md")); err != nil || string(raw) != "Always end with MARKER-ALPHA." {
		t.Fatalf("AGENTS.md = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root.dir, ".opencode", "skills", "marker-skill", "SKILL.md")); err != nil || !strings.Contains(string(raw), "MARKER-BETA") {
		t.Fatalf("SKILL.md = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root.dir, ".opencode", "skills", "marker-skill", "assets", "blob.bin")); err != nil || string(raw) != "\x00\x01\x02" {
		t.Fatalf("blob = %q, %v", raw, err)
	}
	// The role files are never deliverables.
	files, err := h.client.ListSessionFiles(h.ctx(), h.ep, ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "AGENTS.md" || strings.HasPrefix(f.Path, ".opencode") {
			t.Fatalf("listed role file %s", f.Path)
		}
	}
}

// meta: the instructions ride session/new _meta.systemPrompt.append; an
// agent advertising mcpCapabilities.http gets the http server with headers.
func TestProfileMetaInstructionsAndHTTPMcp(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_MCP_HTTP": "1"})
	agent.Profile = &ProfileConfig{InstructionsVia: "meta", SkillsDir: ".claude/skills"}
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, root := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "profile", sampleWireProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	got := h.lastText(ws, sid)
	if !strings.Contains(got, `meta={"systemPrompt":{"append":"Always end with MARKER-ALPHA."}}`) {
		t.Fatalf("meta = %s", got)
	}
	if !strings.Contains(got, `{"type":"http","name":"room","url":"http://127.0.0.1:9/mcp","headers":[{"name":"Authorization","value":"Bearer secret-hdr"}]}`) {
		t.Fatalf("http server = %s", got)
	}
	if _, err := os.Stat(filepath.Join(root.dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("meta mapping wrote AGENTS.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.dir, ".claude", "skills", "marker-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

// env:NAME puts the instructions in the session's agent process env; mcp
// false keeps only the agent's configured servers.
func TestProfileEnvInstructionsAndMcpOff(t *testing.T) {
	off := false
	agent := fakeAgentConfig(t, "fake", nil)
	agent.Profile = &ProfileConfig{InstructionsVia: "env:AGENT_ROLE", MCP: &off}
	agent.McpServers = json.RawMessage(`[{"name":"own","command":"own-mcp","args":[],"env":[]}]`)
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "profile AGENT_ROLE", sampleWireProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	got := h.lastText(ws, sid)
	if !strings.Contains(got, "AGENT_ROLE=Always end with MARKER-ALPHA.") {
		t.Fatalf("env = %s", got)
	}
	if !strings.Contains(got, `mcp=[{"name":"own"`) || strings.Contains(got, "mcp-tools") {
		t.Fatalf("mcp = %s", got)
	}
}

// A profile server named like a configured one is refused (bad_request).
func TestProfileMcpNameClashIsRefused(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", nil)
	agent.McpServers = json.RawMessage(`[{"name":"tools","command":"own","args":[],"env":[]}]`)
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, _ := h.workspace()
	if _, perr := h.sessionWithProfile(ws, "fake", "hi", sampleWireProfile()); perr == nil || perr.Code != protocol.ErrorBadRequest || !strings.Contains(perr.Message, "tools") {
		t.Fatalf("clash = %+v", perr)
	}
	bad := sampleWireProfile()
	bad["skills"] = []map[string]any{{"name": "../x", "files": []any{}}}
	if _, perr := h.sessionWithProfile(ws, "fake", "hi", bad); perr == nil || perr.Code != protocol.ErrorBadRequest {
		t.Fatalf("invalid profile = %+v", perr)
	}
}

// An agent without a profile block gets AGENTS.md + .agents/skills.
func TestProfileDefaultMapping(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, root := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "hello", sampleWireProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	if _, err := os.Stat(filepath.Join(root.dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root.dir, ".agents", "skills", "marker-skill", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

// A session continued in a new agent process keeps its profile: files are
// re-materialized, and env, _meta and MCP servers reach the new process.
func TestProfileSurvivesResume(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_RESUME": "1"})
	agent.Profile = &ProfileConfig{InstructionsVia: "meta", SkillsDir: ".agents/skills"}
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, root := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", "hello", sampleWireProfile())
	if perr != nil {
		t.Fatal(perr)
	}
	h.settle(ws, sid)
	skill := filepath.Join(root.dir, ".agents", "skills", "marker-skill", "SKILL.md")
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	h.a.mu.Lock()
	s := h.a.sessions[sid]
	h.a.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	h.a.reapOnce(time.Millisecond)
	h.send(ws, sid, "profile PROFILE_TOKEN", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("resumed turn failed: %+v", ts)
	}
	s.mu.Lock()
	running := s.proc != nil
	s.mu.Unlock()
	if !running {
		t.Fatal("session did not continue")
	}
	got := h.lastText(ws, sid)
	if !strings.Contains(got, "PROFILE_TOKEN=tok-1") || !strings.Contains(got, "MARKER-ALPHA") || !strings.Contains(got, "mcp-tools") {
		t.Fatalf("resumed profile = %s", got)
	}
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("skill not re-materialized: %v", err)
	}
}

// The bridge writes agent-config files; anything else outside .room/ is
// refused; the agent itself cannot edit them.
func TestWorkspaceWritesWidenToAgentConfigPaths(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, root := h.workspace()
	for _, p := range []string{"AGENTS.md", "CLAUDE.md", ".claude/skills/a/SKILL.md", ".agents/skills/a/SKILL.md", ".opencode/opencode.json", ".mcp.json"} {
		if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, p, "x"); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	var perr *protocol.ProtocolError
	for _, p := range []string{"README.md", "sub/AGENTS.md", ".claude/settings.json", ".agents/other/x"} {
		if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, p, "x"); !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
			t.Fatalf("write %s = %v", p, err)
		}
	}
	sid := h.session(ws, "fake", "write "+filepath.Join(root.dir, "AGENTS.md")+" HIJACK", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "read-only") {
		t.Fatalf("agent write to AGENTS.md = %s", got)
	}
	h.send(ws, sid, "permission edit "+filepath.Join(root.dir, ".claude", "skills", "a", "SKILL.md"), core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, `"optionId":"no"`) {
		t.Fatalf("edit permission on a skill = %s", got)
	}
	h.send(ws, sid, "permission read "+filepath.Join(root.dir, "AGENTS.md"), core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, `"optionId":"yes"`) {
		t.Fatalf("read permission on AGENTS.md = %s", got)
	}
}

// Each target reports the MCP transports its agent accepts: stdio always,
// http/sse from the agent's initialize mcpCapabilities.
func TestTargetsReportMCPTransports(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{
		fakeAgentConfig(t, "plain", nil),
		fakeAgentConfig(t, "web", map[string]string{"ACP_FAKE_MCP_HTTP": "1"}),
	}})
	resp, err := h.srv.Client().Get(h.srv.URL + protocol.PathTargets)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out protocol.ListTargetsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tg := range out.Targets {
		got[tg.ID] = strings.Join(tg.MCPTransports, ",")
	}
	if got["plain"] != "stdio" || got["web"] != "stdio,http,sse" {
		t.Fatalf("transports = %v", got)
	}
}

// The workspace outlives its sessions: each session's profile is
// materialized afresh, so a later profile leaves none of an earlier one's
// skills or instructions behind (also across agents with other mappings and
// for a session without a profile), and the agent's own files stay.
func TestLaterProfileReplacesTheEarlierOne(t *testing.T) {
	a := fakeAgentConfig(t, "a", nil)
	a.Profile = &ProfileConfig{InstructionsVia: "file:AGENTS.md", SkillsDir: ".agents/skills"}
	b := fakeAgentConfig(t, "b", nil)
	b.Profile = &ProfileConfig{InstructionsVia: "file:CLAUDE.md", SkillsDir: ".claude/skills"}
	h := newHarness(t, Config{Agents: []AgentConfig{a, b}})
	ws, root := h.workspace()
	project := map[string]string{"README.md": "mine", "src/app.go": "package app", ".agents/notes.txt": "mine too"}
	for p, c := range project {
		full := filepath.Join(root.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	skill := func(name string) map[string]any {
		return map[string]any{"name": name, "files": []map[string]any{
			{"path": "SKILL.md", "content": "---\nname: " + name + "\ndescription: d\n---\n"},
			{"path": "ref/notes.md", "content": name}}}
	}
	run := func(target string, prof map[string]any) {
		t.Helper()
		sid, perr := h.sessionWithProfile(ws, target, "hello", prof)
		if perr != nil {
			t.Fatal(perr)
		}
		if ts := h.settle(ws, sid); ts.Failed {
			t.Fatalf("turn failed: %+v", ts)
		}
	}
	read := func(rel string) (string, bool) {
		raw, err := os.ReadFile(filepath.Join(root.dir, filepath.FromSlash(rel)))
		return string(raw), err == nil
	}
	absent := func(rels ...string) {
		t.Helper()
		for _, rel := range rels {
			if _, err := os.Lstat(filepath.Join(root.dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
				t.Fatalf("%s is still there (%v)", rel, err)
			}
		}
	}
	projectIntact := func() {
		t.Helper()
		for p, c := range project {
			if got, ok := read(p); !ok || got != c {
				t.Fatalf("project file %s = %q, %v", p, got, ok)
			}
		}
	}

	run("a", map[string]any{"instructions": "role A", "skills": []map[string]any{skill("x"), skill("y")}})
	if got, _ := read("AGENTS.md"); got != "role A" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if _, ok := read(".agents/skills/x/ref/notes.md"); !ok {
		t.Fatal("skill x not written")
	}
	// A skill file written into the skills directory by anyone else goes too:
	// the directory holds exactly the profile's skills.
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, ".agents/skills/stray/SKILL.md", "x"); err != nil {
		t.Fatal(err)
	}

	run("a", map[string]any{"instructions": "role B", "skills": []map[string]any{skill("y")}})
	absent(".agents/skills/x", ".agents/skills/stray")
	if got, _ := read("AGENTS.md"); got != "role B" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if got, ok := read(".agents/skills/y/ref/notes.md"); !ok || got != "y" {
		t.Fatalf("skill y = %q, %v", got, ok)
	}
	projectIntact()

	// Another agent's mapping: the earlier profile's files (by the
	// workspace's record) are removed, the new ones written.
	run("b", map[string]any{"instructions": "role C", "skills": []map[string]any{skill("z")}})
	absent("AGENTS.md", ".agents/skills")
	if got, _ := read("CLAUDE.md"); got != "role C" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	if _, ok := read(".claude/skills/z/SKILL.md"); !ok {
		t.Fatal("skill z not written")
	}
	projectIntact()

	// A profile without instructions or skills, then no profile at all.
	run("b", map[string]any{"env": map[string]string{"K": "v"}})
	absent("CLAUDE.md", ".claude/skills/z")
	run("b", map[string]any{"instructions": "role D"})
	h.settle(ws, h.session(ws, "b", "hello", core.SessionOptions{}))
	absent("CLAUDE.md", ".claude/skills")
	if _, err := os.Stat(root.profileRecordPath()); !os.IsNotExist(err) {
		t.Fatalf("profile record kept after a session without profile: %v", err)
	}
	projectIntact()
}

// Clearing never follows a symlink into the agent's files: a skills
// directory replaced by a link is removed as a link, and a skills directory
// reached through a linked parent is refused.
func TestProfileClearNeverFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	wsDir := filepath.Join(t.TempDir(), "0123456789abcdef")
	if err := os.Mkdir(wsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := newWorkspaceRoot(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root.dir, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "app.go"), []byte("package app"), 0o600); err != nil {
		t.Fatal(err)
	}
	pc := ProfileConfig{InstructionsVia: viaMeta, SkillsDir: ".agents/skills"}
	prof := &core.AgentProfile{Skills: []core.ProfileSkill{{Name: "x", Files: []core.ProfileFile{{Path: "SKILL.md", Content: []byte("x")}}}}}

	if err := os.MkdirAll(filepath.Join(root.dir, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(root.dir, ".agents", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := materializeProfile(prof, root, pc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "app.go")); err != nil {
		t.Fatalf("project file removed through a linked skills dir: %v", err)
	}
	if st, err := os.Lstat(filepath.Join(root.dir, ".agents", "skills")); err != nil || !st.IsDir() {
		t.Fatalf("skills dir = %v, %v (want a real directory)", st, err)
	}

	if err := os.RemoveAll(filepath.Join(root.dir, ".agents")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(src, filepath.Join(root.dir, ".agents")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "skills", "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := materializeProfile(prof, root, pc); !errors.Is(err, errOutsideWorkspace) {
		t.Fatalf("clear through a linked parent = %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "skills", "keep")); err != nil {
		t.Fatalf("project dir removed through a linked parent: %v", err)
	}
}
