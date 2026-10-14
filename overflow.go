package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// A conversation can outgrow the model's context window: the agent then
// fails every turn it is asked to continue. Such a failure is reported as
// context_overflow, and the next turn of the adapter session starts a fresh
// ACP session in the same workspace (the protocol session id is unchanged);
// the overflowing session is never resumed again. The workspace and the
// room's records in it carry what the new session needs.

// failContextOverflow is a turn whose conversation outgrew the model's
// context window (bnbloc's protocol predates the code; the adapter declares
// it like the other live-turn fields).
const failContextOverflow = "context_overflow"

// contextOverflowMessage is the room-visible reason of such a turn.
const contextOverflowMessage = "the conversation outgrew the model's context window; the next message starts a fresh session"

// overflowMarks are what an agent's or a provider's error says when the
// conversation is longer than the model's context window.
var overflowMarks = []string{
	"context window",
	"context length",
	"contextwindowexceeded",
	"prompt is too long",
	"maximum context",
	"too many tokens",
	"longer than the model's context",
}

// contextOverflow reports whether an error text reads as a context-window
// overflow.
func contextOverflow(text string) bool {
	t := strings.ToLower(text)
	for _, m := range overflowMarks {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// startFresh opens a new ACP session for the adapter session in the running
// agent (or a new agent process), replacing the one that overflowed. It
// keeps the profile, mode and options of the session.
func (s *session) startFresh() (*agentProc, error) {
	ctx, cancel := context.WithTimeout(s.ctx, handshakeTimeout)
	defer cancel()
	pc := s.agent.effectiveProfileConfig()
	if err := materializeProfile(s.profile, s.ws, pc); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	started := false
	if proc == nil || !proc.alive() {
		var err error
		if proc, err = startAgent(ctx, s.agent, s.agent.spawnArgs(s.spawnModel), s.ws.dir, s, profileEnviron(s.profile, pc)...); err != nil {
			return nil, err
		}
		started = true
	}
	servers, err := profileMCPServers(s.profile, s.agent, proc.init)
	if err == nil {
		var resp acpSessionResponse
		if err = proc.call(ctx, methodSessionNew, acpNewSessionRequest{Cwd: s.ws.dir, McpServers: servers, Meta: profileMeta(s.profile, pc)}, &resp); err == nil && resp.SessionID == "" {
			err = errors.New("session/new returned no session id")
		}
		if err == nil {
			s.mu.Lock()
			old := s.acpID
			s.proc, s.acpID, s.fresh = proc, resp.SessionID, false
			s.resumable, s.loadable = proc.init.canResume(), proc.init.AgentCapabilities.LoadSession
			if len(resp.ConfigOptions) > 0 {
				s.options = resp.ConfigOptions
			}
			s.mu.Unlock()
			if started {
				go s.watch(proc)
			}
			if err = s.applyMode(ctx, proc, resp.Modes); err == nil {
				slog.Info("bnbloc-acp started a fresh session after a context overflow", "agent", s.agent.Name, "session", s.id, "previous", old)
				return proc, nil
			}
			return nil, fmt.Errorf("%w: %v", errSessionOption, err)
		}
	}
	if started {
		proc.stop()
	}
	return nil, fmt.Errorf("fresh session: %w", err)
}
