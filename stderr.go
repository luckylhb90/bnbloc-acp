package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Some agents report a state of the running turn only in their log, never
// over ACP. OpenCode is one: when the model provider refuses a call (a rate
// limit, an overloaded provider) it waits and retries on its own, honouring
// the provider's retry-after, and sends the client nothing meanwhile, so the
// room saw a running turn that did nothing for many minutes. An agent's
// stderrActivity rules turn such log lines into a step of the running turn
// (live.go), so the room sees why it waits.
//
// The step's title is the rule's own text, never the matched line: stderr is
// arbitrary local output (paths, keys in a provider's error) and stays local.

// StderrRule is one agents.json stderrActivity entry: a line of the agent's
// stderr matching Match (an RE2 regular expression) during a turn adds the
// step Title to the turn's activity.
type StderrRule struct {
	Match string `json:"match"`
	Title string `json:"title"`

	re *regexp.Regexp
}

func (r *StderrRule) compile() error {
	if strings.TrimSpace(r.Match) == "" {
		return errors.New("match is required")
	}
	re, err := regexp.Compile(r.Match)
	if err != nil {
		return fmt.Errorf("match: %w", err)
	}
	t := strings.TrimSpace(r.Title)
	if t == "" || strings.ContainsAny(t, "\r\n") || utf8.RuneCountInString(t) > maxActivityTitle {
		return fmt.Errorf("title: one line of 1 to %d characters", maxActivityTitle)
	}
	r.re, r.Title = re, t
	return nil
}

// stderrStep is the step title of the agent's first rule matching a stderr
// line ("" = none).
func (a AgentConfig) stderrStep(line string) string {
	for _, r := range a.StderrActivity {
		if r.re != nil && r.re.MatchString(line) {
			return r.Title
		}
	}
	return ""
}

// stderrObserver is an rpcHandler that also reads the agent's stderr lines.
type stderrObserver interface {
	agentStderr(line string)
}

// agentStderr adds the step of a matching stderr line to the running turn.
func (s *session) agentStderr(line string) {
	title := s.agent.stderrStep(line)
	if title == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running && !s.loading && !s.startedAt.IsZero() {
		s.noteActivity(activityTool, title, "")
	}
}
