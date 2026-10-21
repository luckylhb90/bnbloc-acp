package main

import (
	"encoding/json"
	"strings"
)

// A session's profile can ask for MCP servers (room tools, the Library's MCP
// tool components) the adapter cannot give the agent (profile.go,
// profileMCPServers). A missing tool must not be silent: the agent would
// believe it has every tool the room's guidance names, and the member would
// watch it fail or improvise. So the next prompt of the ACP session tells the
// agent which tools it does not have and why, and the turn's activity shows
// the room the same (a step "Tools not attached: …").

// sessionMCPServers is the mcpServers of the session's next ACP session
// (session/new, resume, load), recording the profile servers it cannot pass.
func (s *session) sessionMCPServers(proc *agentProc, pc ProfileConfig) (json.RawMessage, error) {
	servers, drops, err := profileMCPServers(s.profile, s.agent, proc.init, s.ws.dir, s.agent.environ(profileEnviron(s.profile, pc)...))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.mcpDrops = drops
	s.mu.Unlock()
	return servers, nil
}

// takeMCPNote is the note for the turn about to be prompted ("" = none): the
// servers the current ACP session lacks, said once per change (a session
// continued with the same servers missing is not told again). It also adds
// the room's activity step.
func (s *session) takeMCPNote() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mcpDropKey(s.mcpDrops)
	if key == s.mcpTold {
		return ""
	}
	s.mcpTold = key
	if len(s.mcpDrops) == 0 {
		return ""
	}
	names := make([]string, len(s.mcpDrops))
	parts := make([]string, len(s.mcpDrops))
	for i, d := range s.mcpDrops {
		names[i] = d.Name
		parts[i] = d.Name + " (" + d.Reason + ")"
	}
	s.noteActivity(activityTool, "Tools not attached: "+strings.Join(names, ", "), "")
	return mcpNote(parts)
}

// mcpNote is the agent-facing note naming the missing servers.
func mcpNote(parts []string) string {
	return "[bnbloc-acp] Tools requested for this session could not be attached, so you do not have them: " +
		strings.Join(parts, "; ") + ". If the request needs them, tell the person so instead of working around them."
}

func mcpDropKey(drops []mcpDrop) string {
	var b strings.Builder
	for _, d := range drops {
		b.WriteString(d.Name + "\x00" + d.Reason + "\x00")
	}
	return b.String()
}
