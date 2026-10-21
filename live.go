package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luckylhb90/bnbloc/core/protocol"
)

// A live turn (bnbloc docs/prd/2026-10-13-live-turns.md): the caller owns how
// long a turn may run (deadlineMs on session create and input), and a running
// turn reports when it started and its latest steps (capability
// turnActivity). The wire names mirror bnbloc's core/protocol
// (CreateSessionRequest.DeadlineMs, PollTurnResponse.FailCode / StartedAtMs /
// Activity); the adapter declares them here so it builds against bnbloc
// versions that predate them.

// capabilityTurnActivity is Core Protocol's turnActivity capability.
const capabilityTurnActivity = "turnActivity"

// deadlineGrace is how long an agent has to end a turn after session/cancel
// at its deadline before its process is stopped.
const deadlineGrace = 30 * time.Second

// Turn activity kinds and bounds.
const (
	activityTool     = "tool"
	activityThinking = "thinking"
	activityText     = "text"
	maxActivity      = 8
	maxActivityTitle = 120
)

// createSessionRequest and sendInputRequest add the caller's deadline to the
// protocol requests.
type createSessionRequest struct {
	protocol.CreateSessionRequest
	DeadlineMs int64 `json:"deadlineMs,omitempty"`
}

type sendInputRequest struct {
	protocol.SendInputRequest
	DeadlineMs int64 `json:"deadlineMs,omitempty"`
}

// deadlineOf is a request's deadline (zero = none given).
func deadlineOf(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// turnActivity is one step of a running turn (protocol TurnActivity).
type turnActivity struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	AtMs  int64  `json:"atMs"`
	// toolCallID ties a tool step to its later updates (never on the wire).
	toolCallID string
}

// deadlineMessage is the room-visible reason of a turn that ran out of time.
func deadlineMessage(started, deadline time.Time) string {
	mins := int(deadline.Sub(started).Round(time.Minute) / time.Minute)
	if mins < 1 {
		return "ran out of time after less than a minute"
	}
	if mins == 1 {
		return "ran out of time after 1 minute"
	}
	return fmt.Sprintf("ran out of time after %d minutes", mins)
}

// watchDeadline cancels the running turn at its deadline (session/cancel,
// marking it deadlineHit) and stops the agent's process if the agent has not
// ended the turn deadlineGrace later. It returns the watch's stop.
func (s *session) watchDeadline(deadline time.Time) (stop func()) {
	var mu sync.Mutex
	var grace *time.Timer
	t := time.AfterFunc(time.Until(deadline), func() {
		s.mu.Lock()
		s.deadlineHit, s.cancelled = true, true
		proc, acpID := s.proc, s.acpID
		s.mu.Unlock()
		if proc == nil {
			return
		}
		_ = proc.conn.Notify(methodSessionCancel, acpCancelNotification{SessionID: acpID})
		mu.Lock()
		grace = time.AfterFunc(deadlineGrace, proc.stop)
		mu.Unlock()
	})
	return func() {
		t.Stop()
		mu.Lock()
		if grace != nil {
			grace.Stop()
		}
		mu.Unlock()
	}
}

// noteActivity records a step of the running turn (s.mu held): a thought or
// text continues the step of its kind, a tool call is its own step (a later
// update of the same call retitles it), a step without a call id (a
// stderrActivity rule's, an MCP notice) continues an equal latest step, and
// only the latest maxActivity steps are kept.
func (s *session) noteActivity(kind, title, toolCallID string) {
	now := time.Now().UnixMilli()
	title = activityTitle(title)
	if toolCallID != "" {
		for i := range s.activity {
			if s.activity[i].toolCallID == toolCallID {
				if title != "" {
					s.activity[i].Title = title
				}
				return
			}
		}
	} else if n := len(s.activity); n > 0 && s.activity[n-1].Kind == kind && s.activity[n-1].toolCallID == "" && (kind != activityTool || s.activity[n-1].Title == title) {
		s.activity[n-1].AtMs = now
		return
	}
	s.activity = append(s.activity, turnActivity{Kind: kind, Title: title, AtMs: now, toolCallID: toolCallID})
	if over := len(s.activity) - maxActivity; over > 0 {
		s.activity = append(s.activity[:0:0], s.activity[over:]...)
	}
}

// toolTitle names a tool call for the room: the agent's own title, else its
// kind and the first location's file name. Never its content.
func toolTitle(u acpSessionUpdate) string {
	if t := strings.TrimSpace(u.Title); t != "" {
		return t
	}
	name := ""
	if len(u.Locations) > 0 && u.Locations[0].Path != "" {
		name = filepath.Base(u.Locations[0].Path)
	}
	switch {
	case u.Kind != "" && name != "":
		return u.Kind + ": " + name
	case u.Kind != "":
		return u.Kind
	default:
		return name
	}
}

// activityTitle is one line of at most maxActivityTitle characters.
func activityTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if utf8.RuneCountInString(t) <= maxActivityTitle {
		return t
	}
	r := []rune(t)
	return string(r[:maxActivityTitle-1]) + "…"
}
