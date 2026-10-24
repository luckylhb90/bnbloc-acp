package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What the live sessions run from, published for whatever manages the agents'
// files (the bnbloc agent manager). A session fixes the program it was created
// with and continues with that program after an idle stop, so a file removed
// while a session is idle loses the conversation (docs/prd/
// 2026-10-24-agent-manager.md §2.4a). The core says which paths its sessions
// hold: `<home>/in-use.json`, rewritten when a session starts or ends and every
// minute while the core runs, and marked stopped when it exits cleanly.
//
// A reader treats the file as current for inUseFresh after its updatedAt (a core
// that was killed leaves it behind, and its sessions are gone with it), and as
// unknown when it does not exist (a core that predates it).
const (
	inUseFile = "in-use.json"
	// inUseFresh is how long a file counts as the core's current word.
	inUseFresh = 5 * time.Minute
)

// inUseDoc is in-use.json.
type inUseDoc struct {
	PID       int   `json:"pid"`
	UpdatedAt int64 `json:"updatedAtMs"`
	// Stopped: the core exited cleanly; no session is alive.
	Stopped  bool           `json:"stopped,omitempty"`
	Sessions []inUseSession `json:"sessions"`
}

// inUseSession is one live session: its agent and every path-like string its
// launch carries (the command, its arguments and its environment values: an
// adapter may be told where its agent is).
type inUseSession struct {
	Agent string   `json:"agent"`
	Refs  []string `json:"refs"`
}

// launchRefs are the strings of an agent's launch that name a file.
func launchRefs(a AgentConfig) []string {
	var out []string
	add := func(s string) {
		if strings.ContainsAny(s, `/\\`) {
			out = append(out, s)
		}
	}
	add(a.Command)
	for _, s := range a.Args {
		add(s)
	}
	for _, v := range a.Env {
		add(v)
	}
	return out
}

// publishInUse writes in-use.json for the sessions that are alive now.
func (a *adapter) publishInUse(stopped bool) {
	a.mu.Lock()
	doc := inUseDoc{PID: os.Getpid(), UpdatedAt: time.Now().UnixMilli(), Stopped: stopped, Sessions: []inUseSession{}}
	if !stopped {
		for _, s := range a.sessions {
			s.mu.Lock()
			doc.Sessions = append(doc.Sessions, inUseSession{Agent: s.agent.Name, Refs: launchRefs(s.agent)})
			s.mu.Unlock()
		}
	}
	a.mu.Unlock()
	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(filepath.Dir(a.ws.root), inUseFile), raw, 0o600); err != nil {
		slog.Warn("bnbloc-acp could not publish what its sessions run from", "error", err)
	}
}
