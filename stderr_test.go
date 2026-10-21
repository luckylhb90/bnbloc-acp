package main

import (
	"strings"
	"testing"
	"time"

	"github.com/luckylhb90/bnbloc/core"
)

// A stderr line matching an agent's stderrActivity rule is a step of the
// running turn, titled by the rule, never by the line.
func TestStderrActivityMarksAStep(t *testing.T) {
	agent := fakeAgentConfig(t, "fake", nil)
	agent.StderrActivity = []StderrRule{
		{Match: `message="stream error".*(?i)rate limit`, Title: "Waiting for the model: rate-limited, the agent retries"},
		{Match: `message="stream error"`, Title: "The model call failed, the agent retries"},
	}
	h := newHarness(t, Config{Agents: []AgentConfig{agent}})
	ws, _ := h.workspace()
	sid, perr := h.sessionWithProfile(ws, "fake", `stderrwait level=ERROR message="stream error" error="AI_APICallError: Rate limit exceeded key=sk-secret-1"`, nil)
	if perr != nil {
		t.Fatal(perr)
	}
	ts := h.waitStep(ws, sid, "tool:Waiting for the model: rate-limited, the agent retries")
	for _, a := range ts.Activity {
		if strings.Contains(a.Title, "sk-secret") || strings.Contains(a.Title, "failed") {
			t.Fatalf("step %+v: only the first matching rule's own title", a)
		}
	}
	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
	h.settle(ws, sid)

	// A line no rule matches adds nothing.
	h.send(ws, sid, "stderrwait level=WARN message=\"duplicate skill name\"", core.SessionOptions{})
	deadline := time.Now().Add(10 * time.Second)
	for {
		ts := h.pollNow(ws, sid)
		if n := len(ts.Activity); n > 0 && ts.Activity[n-1].Kind == activityText {
			for _, a := range ts.Activity {
				if a.Kind == activityTool {
					t.Fatalf("unmatched line made a step: %+v", ts.Activity)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn never ran: %+v", ts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the stderr line has been read by now
	for _, a := range h.pollNow(ws, sid).Activity {
		if a.Kind == activityTool {
			t.Fatalf("unmatched line made a step: %+v", a)
		}
	}
	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
}

func TestStderrActivityValidates(t *testing.T) {
	long := strings.Repeat("x", maxActivityTitle+1)
	cases := map[string]string{
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"(","title":"t"}]}]}`:            "match",
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"","title":"t"}]}]}`:             "match is required",
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"x","title":" "}]}]}`:            "title",
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"x","title":"a\nb"}]}]}`:         "title",
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"x","title":"` + long + `"}]}]}`: "title",
		`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"x","title":"t","x":1}]}]}`:      "unknown field",
	}
	for raw, want := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseConfig(%s) = %v, want %q", raw, err, want)
		}
	}
	c, err := ParseConfig([]byte(`{"agents":[{"name":"a","command":"x","stderrActivity":[{"match":"rate limit","title":"Waiting"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Agent("a")
	if a.stderrStep("ERROR rate limit") != "Waiting" || a.stderrStep("ok") != "" {
		t.Fatal("rule does not match")
	}
}
