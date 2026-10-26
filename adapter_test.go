package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/conformance"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// fakeAgent returns an agent config that runs this test binary as the fake
// ACP agent (fakeagent_test.go) with the given switches.
func fakeAgentConfig(t *testing.T, name string, env map[string]string) AgentConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// GORACE: a -race test binary otherwise sleeps a second at every exit.
	e := map[string]string{"ACP_FAKE_AGENT": "1", "GORACE": "atexit_sleep_ms=0"}
	for k, v := range env {
		e[k] = v
	}
	return AgentConfig{Name: name, Command: exe, Env: e}
}

type harness struct {
	t      *testing.T
	a      *adapter
	srv    *httptest.Server
	client *protocol.Client
	ep     core.Endpoint
	home   string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return newHarnessAt(t, cfg, t.TempDir())
}

// newHarnessAt is newHarness with the adapter's home in a given directory.
func newHarnessAt(t *testing.T, cfg Config, home string) *harness {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "agents.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(path, probeDirFor(home))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := newWorkspaces(filepath.Join(home, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	a := newAdapter("acp", "", ws, reg)
	h := &harness{t: t, a: a, home: home}
	h.probeAll()
	h.srv = httptest.NewServer(a.handler())
	h.client = protocol.NewClient("acp", h.srv.Client())
	h.ep = core.Endpoint{BaseURL: h.srv.URL}
	t.Cleanup(func() {
		h.srv.Close()
		a.close()
	})
	return h
}

// probeAll probes every configured agent synchronously.
func (h *harness) probeAll() {
	reg := h.a.reg
	for _, ag := range reg.config().Agents {
		st := reg.probe(context.Background(), ag)
		reg.mu.Lock()
		reg.status[ag.Name] = st
		reg.mu.Unlock()
	}
}

func (h *harness) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	h.t.Cleanup(cancel)
	return ctx
}

func (h *harness) workspace() (string, workspaceRoot) {
	h.t.Helper()
	id, err := h.client.CreateWorkspace(h.ctx(), h.ep)
	if err != nil {
		h.t.Fatal(err)
	}
	ws, err := h.a.ws.get(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return id, ws
}

func (h *harness) session(ws, target, prompt string, opts core.SessionOptions) string {
	h.t.Helper()
	res, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: target, Prompt: prompt, Options: opts})
	if err != nil {
		h.t.Fatalf("CreateSession: %v", err)
	}
	return res.SessionID
}

func (h *harness) send(ws, sid, text string, opts core.SessionOptions) {
	h.t.Helper()
	if err := h.client.SendInput(h.ctx(), h.ep, ws, sid, text, opts); err != nil {
		h.t.Fatalf("SendInput: %v", err)
	}
}

// settle waits until the session's turn is idle and returns its status.
func (h *harness) settle(ws, sid string) core.TurnStatus {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ts, err := h.client.PollTurn(h.ctx(), h.ep, ws, sid)
		if err != nil {
			h.t.Fatalf("PollTurn: %v", err)
		}
		if ts.Idle {
			return ts
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatal("session never settled")
	return core.TurnStatus{}
}

func (h *harness) messages(ws, sid string, after int64) []core.Message {
	h.t.Helper()
	msgs, _, err := h.client.Messages(h.ctx(), h.ep, ws, sid, after)
	if err != nil {
		h.t.Fatal(err)
	}
	return msgs
}

func (h *harness) lastText(ws, sid string) string {
	h.t.Helper()
	msgs := h.messages(ws, sid, 0)
	if len(msgs) == 0 {
		h.t.Fatal("no messages")
	}
	return msgs[len(msgs)-1].Text
}

func texts(msgs []core.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Text
	}
	return out
}

func TestConformance(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	conformance.RunLive(t, conformance.Config{Endpoint: h.ep, HTTPClient: h.srv.Client(), TargetID: "fake", Timeout: time.Minute})
}

func TestTargetsComeFromTheAgentAndUnavailableAgentsAreListedOnly(t *testing.T) {
	missing := AgentConfig{Name: "missing", Command: "bnbloc-acp-no-such-agent"}
	authed := fakeAgentConfig(t, "needs-auth", map[string]string{"ACP_FAKE_AUTH": "stored-login"})
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil), missing, authed}})

	targets, err := h.client.ListTargets(h.ctx(), h.ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only the available fake", targets)
	}
	got := targets[0]
	if got.ID != "fake" || got.Provider != "fake" || strings.Join(got.Models, ",") != "fake-small,fake-large" || got.DefaultModel != "fake-small" ||
		strings.Join(got.ReasoningEfforts, ",") != "low,high" || got.DefaultReasoning != "low" {
		t.Fatalf("target = %+v", got)
	}

	resp, err := h.srv.Client().Get(h.srv.URL + "/acp/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Agents []agentStatus `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 3 || body.Agents[1].Available || !strings.Contains(body.Agents[1].Reason, "not found") ||
		body.Agents[2].Available || !strings.Contains(body.Agents[2].Reason, "authentication required") {
		t.Fatalf("agents = %+v", body.Agents)
	}

	// Creating a session on the missing agent is a graceful core_unavailable.
	ws, _ := h.workspace()
	_, err = h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "missing", Prompt: "hi"})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorCoreUnavailable {
		t.Fatalf("CreateSession(missing) = %v, want core_unavailable", err)
	}
	_, err = h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "nope", Prompt: "hi"})
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorTargetNotFound {
		t.Fatalf("CreateSession(nope) = %v, want target_not_found", err)
	}
}

func TestAgentStderrStaysLocal(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "dies", map[string]string{"ACP_FAKE_DIE": "1"})}})
	st := h.a.reg.statuses()[0]
	if st.Available || !strings.Contains(st.Reason, "exited") || !strings.Contains(st.Reason, "fatal: local detail") {
		t.Fatalf("probe reason = %q, want the exit with its stderr line", st.Reason)
	}
	ws, _ := h.workspace()
	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "dies", Prompt: "hi"})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorCoreUnavailable || strings.Contains(perr.Message, "fatal") {
		t.Fatalf("CreateSession = %v, want core_unavailable without the agent's stderr", err)
	}
}

func TestAuthMethodIsPassedToAuthenticate(t *testing.T) {
	a := fakeAgentConfig(t, "authed", map[string]string{"ACP_FAKE_AUTH": "stored-login"})
	a.AuthMethod = "stored-login"
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	if targets, _ := h.client.ListTargets(h.ctx(), h.ep); len(targets) != 1 {
		t.Fatalf("targets = %+v, want the authenticated agent", targets)
	}
}

func TestMessagesAggregateIntoCompletedReplies(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("turn failed: %+v", ts)
	}
	msgs := h.messages(ws, sid, 0)
	if len(msgs) != 1 || msgs[0].Text != "hello world" || !msgs[0].Completed || !msgs[0].UserVisible || msgs[0].Role != "assistant" || msgs[0].MessageID != "m" {
		t.Fatalf("messages = %+v", msgs)
	}
	m := msgs[0].Metrics
	if m["model"] != "fake-small" || m["inputTokens"] != float64(3) || m["outputTokens"] != float64(2) || m["costUsd"] != 0.25 {
		t.Fatalf("metrics = %+v", m)
	}
	// The agent's usage_update (used 100 of 1000) is how full its context is.
	if m["contextTokens"] != float64(100) || m["contextWindow"] != float64(1000) {
		t.Fatalf("context metrics = %v of %v, want 100 of 1000", m["contextTokens"], m["contextWindow"])
	}

	// A tool call between texts completes the first message; usage rides
	// the turn's last message; cost is the turn's delta.
	h.send(ws, sid, "two", core.SessionOptions{})
	h.settle(ws, sid)
	two := h.messages(ws, sid, msgs[0].Version)
	if strings.Join(texts(two), "|") != "first|second" || two[0].Metrics != nil || two[1].Metrics["costUsd"] != 0.25 {
		t.Fatalf("two = %+v", two)
	}
	if two[0].Version <= msgs[0].Version || two[1].Seq != two[1].Version {
		t.Fatalf("versions not monotonic: %+v", two)
	}

	// A messageId change starts a new message; a thought rides the next one.
	h.send(ws, sid, "ids", core.SessionOptions{})
	h.settle(ws, sid)
	h.send(ws, sid, "think", core.SessionOptions{})
	h.settle(ws, sid)
	rest := h.messages(ws, sid, two[1].Version)
	if strings.Join(texts(rest), "|") != "alpha|beta|answer" || rest[2].Thinking != "pondering" {
		t.Fatalf("rest = %+v", rest)
	}
}

func TestInputDuringATurnIsQueued(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "two", core.SessionOptions{})
	h.send(ws, sid, "hello", core.SessionOptions{})
	h.send(ws, sid, "ids", core.SessionOptions{})
	h.settle(ws, sid)
	if got := strings.Join(texts(h.messages(ws, sid, 0)), "|"); got != "first|second|hello world|alpha|beta" {
		t.Fatalf("messages = %s", got)
	}
}

func TestInterruptCancelsTheTurnAndDropsQueuedInput(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "slow", core.SessionOptions{})
	h.send(ws, sid, "hello", core.SessionOptions{})
	time.Sleep(100 * time.Millisecond)
	if ts, _ := h.client.PollTurn(h.ctx(), h.ep, ws, sid); ts.Idle {
		t.Fatal("a running turn reported idle")
	}
	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
	ts := h.settle(ws, sid)
	if ts.Failed {
		t.Fatalf("an interrupted turn is not a failure: %+v", ts)
	}
	if got := strings.Join(texts(h.messages(ws, sid, 0)), "|"); got != "working" {
		t.Fatalf("messages = %s (queued input must be dropped)", got)
	}
	// Interrupt without a running turn is a no-op.
	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
}

func TestFailedTurnsAreReported(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "fail", core.SessionOptions{})
	if ts := h.settle(ws, sid); !ts.Failed || !strings.Contains(ts.FailMessage, "boom") {
		t.Fatalf("status = %+v", ts)
	}
	h.send(ws, sid, "refuse", core.SessionOptions{})
	if ts := h.settle(ws, sid); !ts.Failed || !strings.Contains(ts.FailMessage, "refused") {
		t.Fatalf("status = %+v", ts)
	}
	// A turn the policy stopped without any reply says why.
	h.send(ws, sid, "quiet", core.SessionOptions{})
	if ts := h.settle(ws, sid); !ts.Failed || !strings.Contains(ts.FailMessage, `rm -rf build (tool kind "execute" is not allowed)`) {
		t.Fatalf("status = %+v", ts)
	}
	h.send(ws, sid, "hello", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("a good turn after a failed one: %+v", ts)
	}
}

// rawPoll waits until the session is idle and reads its turn status over the
// raw wire (failCode is not in the typed client's TurnStatus).
func (h *harness) rawPoll(ws, sid string) turnStatus {
	h.t.Helper()
	// One poll that reports idle is the status the caller reads: a failed
	// idle poll is the one that marks a lost session's failure as seen.
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := h.srv.Client().Get(h.srv.URL + "/core/v1/workspaces/" + ws + "/sessions/" + sid + "/turn")
		if err != nil {
			h.t.Fatal(err)
		}
		var ts turnStatus
		ok := resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&ts) == nil
		resp.Body.Close()
		if !ok {
			h.t.Fatalf("PollTurn: HTTP %d", resp.StatusCode)
		}
		if ts.Idle {
			return ts
		}
		if time.Now().After(deadline) {
			h.t.Fatal("session never settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An errored turn is a failed turn, never a reply: a JSON-RPC error, an
// error stop reason and the agent exiting mid-turn each fail the turn with
// its message and code, and the text streamed before the error never
// becomes a completed message (nor leaks into the next turn's reply).
func TestErroredTurnIsFailedNotAReply(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	_, before, err := h.client.Messages(h.ctx(), h.ep, ws, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ prompt, msg string }{
		{"failafter", "the agent returned an error: provider overloaded"},
		{"stoperror", "the agent ended the turn with an error"},
		{"systemerror", "the agent returned an error: unexpected status 403 Forbidden: model not allowed"},
	} {
		h.send(ws, sid, c.prompt, core.SessionOptions{})
		ts := h.rawPoll(ws, sid)
		if !ts.Idle || !ts.Failed || ts.FailMessage != c.msg || ts.FailCode != failAgentError {
			t.Fatalf("%s: status = %+v", c.prompt, ts)
		}
		if msgs := h.messages(ws, sid, before); len(msgs) != 0 {
			t.Fatalf("%s: the failed turn served messages %q", c.prompt, texts(msgs))
		}
	}
	// A reconnect the agent recovers from is a reply, not a failure.
	_, mark, _ := h.client.Messages(h.ctx(), h.ep, ws, sid, 0)
	h.send(ws, sid, "recovered", core.SessionOptions{})
	if ts := h.rawPoll(ws, sid); ts.Failed {
		t.Fatalf("recovered: status = %+v", ts)
	}
	if msgs := h.messages(ws, sid, mark); len(msgs) != 1 || msgs[0].Text != "recovered answer" {
		t.Fatalf("recovered: messages %q", texts(msgs))
	}
	_, before, _ = h.client.Messages(h.ctx(), h.ep, ws, sid, 0)
	h.send(ws, sid, "hello", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("a good turn after failed ones: %+v", ts)
	}
	msgs := h.messages(ws, sid, before)
	if len(msgs) != 1 || msgs[0].Text != "hello world" || msgs[0].Thinking != "" {
		t.Fatalf("next reply = %+v (the failed turns' text or thinking leaked into it)", msgs)
	}

	// The agent process exiting mid-turn (an agent that cannot resume).
	sid = h.session(ws, "fake", "crashafter", core.SessionOptions{})
	ts := h.rawPoll(ws, sid)
	if !ts.Failed || ts.FailMessage != "the agent process exited during the turn" || ts.FailCode != failAgentExited {
		t.Fatalf("crash: status = %+v", ts)
	}
	if msgs := h.messages(ws, sid, 0); len(msgs) != 0 {
		t.Fatalf("crash: the failed turn served messages %q", texts(msgs))
	}
}

func TestModelReasoningAndModeAreApplied(t *testing.T) {
	a := fakeAgentConfig(t, "fake", nil)
	a.Mode = "accept"
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "whoami", core.SessionOptions{Model: "fake-large", ReasoningEffort: "high"})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "model=fake-large effort=high mode=accept") {
		t.Fatalf("whoami = %q", got)
	}
	h.send(ws, sid, "whoami", core.SessionOptions{Model: "fake-small"})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "model=fake-small effort=high") {
		t.Fatalf("whoami after switch = %q", got)
	}
	msgs := h.messages(ws, sid, 0)
	if msgs[len(msgs)-1].Metrics["model"] != "fake-small" {
		t.Fatalf("metrics = %+v", msgs[len(msgs)-1].Metrics)
	}

	_, err := h.client.CreateSession(h.ctx(), h.ep, core.SessionRequest{WorkspaceID: ws, TargetID: "fake", Prompt: "hi", Options: core.SessionOptions{Model: "no-such-model"}})
	var perr *protocol.ProtocolError
	if !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
		t.Fatalf("CreateSession(bad model) = %v, want bad_request", err)
	}
}

func TestConfigDeclaredModelsApplyThroughModelArgs(t *testing.T) {
	a := fakeAgentConfig(t, "plain", map[string]string{"ACP_FAKE_NO_MODELS": "1"})
	a.Models, a.ModelArgs = []string{"m1", "m2"}, []string{"--model", "{model}"}
	h := newHarness(t, Config{Agents: []AgentConfig{a}})
	targets, err := h.client.ListTargets(h.ctx(), h.ep)
	if err != nil || len(targets) != 1 || strings.Join(targets[0].Models, ",") != "m1,m2" {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	ws, _ := h.workspace()
	sid := h.session(ws, "plain", "whoami", core.SessionOptions{Model: "m2"})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.HasSuffix(got, "args=--model m2") {
		t.Fatalf("whoami = %q", got)
	}
}

func TestPermissionPolicy(t *testing.T) {
	open := fakeAgentConfig(t, "open", nil)
	open.Permissions.Allow = []string{"*"}
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil), open}})
	ws, root := h.workspace()
	cases := []struct {
		agent, prompt, want string
	}{
		{"fake", "permission edit " + filepath.Join(root.dir, "a.txt"), `"optionId":"yes"`},
		{"fake", "permission read docs/notes.md", `"optionId":"yes"`},
		{"fake", "permission execute", `"optionId":"no"`},
		{"fake", "permission fetch", `"optionId":"no"`},
		{"fake", "permission edit /etc/hosts", `"optionId":"no"`},
		{"fake", "permission edit " + filepath.Join(root.dir, "..", "escape.txt"), `"optionId":"no"`},
		{"open", "permission execute", `"optionId":"yes"`},
		{"open", "permission edit /etc/hosts", `"optionId":"no"`},
	}
	for _, c := range cases {
		sid := h.session(ws, c.agent, c.prompt, core.SessionOptions{})
		h.settle(ws, sid)
		if got := h.lastText(ws, sid); !strings.Contains(got, c.want) {
			t.Errorf("%s %q -> %s, want %s", c.agent, c.prompt, got, c.want)
		}
	}
}

func TestFileSystemIsConfinedToTheWorkspace(t *testing.T) {
	readOnly := fakeAgentConfig(t, "reader", nil)
	readOnly.Permissions.Allow = []string{"read"}
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil), readOnly}})
	ws, root := h.workspace()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.dir, "lines.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(agent, prompt string) string {
		sid := h.session(ws, agent, prompt, core.SessionOptions{})
		h.settle(ws, sid)
		return h.lastText(ws, sid)
	}
	if got := run("fake", "write "+filepath.Join(root.dir, "out", "new.txt")+" hello"); got != "write={}" {
		t.Fatalf("write in workspace = %s", got)
	}
	if raw, err := os.ReadFile(filepath.Join(root.dir, "out", "new.txt")); err != nil || string(raw) != "hello" {
		t.Fatalf("written file = %q, %v", raw, err)
	}
	for _, p := range []string{outside, filepath.Join(root.dir, "link", "x.txt"), filepath.Join(root.dir, ".room", "x.md"), "relative.txt"} {
		if got := run("fake", "write "+p+" evil"); !strings.Contains(got, "__error") {
			t.Errorf("write %s = %s, want an error", p, got)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a write escaped the workspace")
	}
	if got := run("fake", "read "+filepath.Join(root.dir, "lines.txt")); !strings.Contains(got, `"content":"two\n"`) {
		t.Fatalf("read with line/limit = %s", got)
	}
	if got := run("reader", "write "+filepath.Join(root.dir, "denied.txt")+" x"); !strings.Contains(got, "not allowed") {
		t.Fatalf("write without edit = %s", got)
	}
}

func TestTerminalsNeedExecute(t *testing.T) {
	runner := fakeAgentConfig(t, "runner", nil)
	runner.Permissions.Allow = []string{"execute"}
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil), runner}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "terminal echo hello-world", core.SessionOptions{})
	h.settle(ws, sid)
	if got := h.lastText(ws, sid); !strings.Contains(got, "not allowed") {
		t.Fatalf("terminal without execute = %s", got)
	}
	sid = h.session(ws, "runner", "terminal echo hello-world", core.SessionOptions{})
	h.settle(ws, sid)
	got := h.lastText(ws, sid)
	// The last 5 bytes of the output: "orld\n", or "rld\r\n" where the shell
	// ends lines with CRLF (Windows).
	want := `"output":"orld\n"`
	if runtime.GOOS == "windows" {
		want = `"output":"rld\r\n"`
	}
	if !strings.Contains(got, `"exitCode":0`) || !strings.Contains(got, want) || !strings.Contains(got, `"truncated":true`) {
		t.Fatalf("terminal = %s", got)
	}
}

func TestCrashedAgentWithoutResumeLosesTheSession(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "crash", core.SessionOptions{})
	ts := h.settle(ws, sid)
	if !ts.Failed || !strings.Contains(ts.FailMessage, "exited") {
		t.Fatalf("status = %+v", ts)
	}
	// The failure was seen; the session is now gone so the bridge rebinds.
	var perr *protocol.ProtocolError
	if _, err := h.client.PollTurn(h.ctx(), h.ep, ws, sid); !errors.As(err, &perr) || perr.Code != protocol.ErrorSessionNotFound {
		t.Fatalf("PollTurn after loss = %v", err)
	}
	if err := h.client.SendInput(h.ctx(), h.ep, ws, sid, "hello", core.SessionOptions{}); !errors.As(err, &perr) || perr.Code != protocol.ErrorSessionNotFound {
		t.Fatalf("SendInput after loss = %v", err)
	}
}

func TestCrashedAgentResumesTheSession(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_RESUME": "1"})}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "whoami", core.SessionOptions{})
	h.settle(ws, sid)
	first := h.lastText(ws, sid)
	acpID := strings.Fields(first)[0]
	h.send(ws, sid, "crash", core.SessionOptions{})
	if ts := h.settle(ws, sid); !ts.Failed {
		t.Fatalf("status = %+v", ts)
	}
	h.send(ws, sid, "whoami", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("resumed turn failed: %+v", ts)
	}
	if got := h.lastText(ws, sid); !strings.HasPrefix(got, acpID+" resumed=true") {
		t.Fatalf("after crash = %q, want %s resumed", got, acpID)
	}
}

func TestIdleAgentIsStoppedAndLoadedAgain(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_LOAD": "1"})}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	h.a.mu.Lock()
	s := h.a.sessions[sid]
	h.a.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	h.a.reapOnce(time.Millisecond)
	s.mu.Lock()
	stopped := s.proc == nil
	s.mu.Unlock()
	if !stopped {
		t.Fatal("idle agent was not stopped")
	}
	h.send(ws, sid, "whoami", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("loaded turn failed: %+v", ts)
	}
	got := strings.Join(texts(h.messages(ws, sid, 0)), "|")
	if strings.Contains(got, "REPLAYED") || !strings.Contains(got, "resumed=true") {
		t.Fatalf("messages = %s (load replay must be discarded)", got)
	}
}

func TestWorkspaceFilesAndRoomContext(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, root := h.workspace()
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, ".room/INDEX.md", "records"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.WriteWorkspaceFileBytes(h.ctx(), h.ep, ws, ".room/attachments/a.bin", []byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	// The client's own guard refuses both before any I/O.
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, "README.md", "x"); !errors.Is(err, core.ErrWorkspacePathNotAllowed) {
		t.Fatalf("write outside .room = %v", err)
	}
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, ".room/../../x", "x"); !errors.Is(err, core.ErrWorkspacePathNotAllowed) {
		t.Fatalf("write escaping = %v", err)
	}
	sid := h.session(ws, "fake", "write "+filepath.Join(root.dir, "out", "report.md")+" REPORT", core.SessionOptions{})
	h.settle(ws, sid)
	for _, p := range []string{"node_modules/x.js", ".git/config", "dist/index.html"} {
		full := filepath.Join(root.dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o700)
		_ = os.WriteFile(full, []byte("x"), 0o600)
	}
	files, err := h.client.ListSessionFiles(h.ctx(), h.ep, ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "dist/index.html,out/report.md" {
		t.Fatalf("listed %v", paths)
	}
	raw, err := h.client.ReadSessionFile(h.ctx(), h.ep, ws, sid, "out/report.md")
	if err != nil || string(raw) != "REPORT" {
		t.Fatalf("read = %q, %v", raw, err)
	}
	var perr *protocol.ProtocolError
	if _, err := h.client.ReadSessionFile(h.ctx(), h.ep, ws, sid, "missing.txt"); !errors.As(err, &perr) || perr.Code != protocol.ErrorFileNotFound {
		t.Fatalf("read missing = %v", err)
	}
	if err := h.client.DeleteWorkspace(h.ctx(), h.ep, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root.dir); !os.IsNotExist(err) {
		t.Fatalf("workspace dir still there: %v", err)
	}
	if _, err := h.client.PollTurn(h.ctx(), h.ep, ws, sid); !errors.As(err, &perr) || perr.Code != protocol.ErrorSessionNotFound {
		t.Fatalf("poll after delete = %v", err)
	}
}

func TestBearerTokenIsRequiredWhenSet(t *testing.T) {
	h := newHarness(t, Config{})
	srv := httptest.NewServer(newAdapter("acp", "s3cret", h.a.ws, h.a.reg).handler()) // gitleaks:allow
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + protocol.PathTargets)
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.ErrorEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || env.Error.Code != protocol.ErrorUnauthorized {
		t.Fatalf("no token = %d %+v", resp.StatusCode, env)
	}
	c := protocol.NewClient("acp", srv.Client())
	if _, err := c.ListTargets(context.Background(), core.Endpoint{BaseURL: srv.URL, Token: "s3cret"}); err != nil { // gitleaks:allow
		t.Fatal(err)
	}
}

func TestAgentEnvironmentNeverCarriesAdapterSettings(t *testing.T) {
	t.Setenv("BNBLOC_ACP_TOKEN", "do-not-leak")
	env := strings.Join(AgentConfig{Env: map[string]string{"X": "1"}}.environ("Y=2"), "\n")
	if strings.Contains(env, "BNBLOC_ACP_TOKEN") || !strings.Contains(env, "X=1") || !strings.Contains(env, "Y=2") {
		t.Fatalf("environ leaked or dropped settings")
	}
}

// rawPost sends a raw protocol request (fields newer than the typed client)
// and returns the answer's body.
func (h *harness) rawPost(path string, body any) []byte {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := h.srv.Client().Post(h.srv.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		h.t.Fatalf("POST %s: HTTP %d %s", path, resp.StatusCode, out)
	}
	return out
}

// A turn still running at its deadline is cancelled and fails with the
// deadline code; the text it was writing is dropped. A turn without one runs
// as before, and so does the next turn after a deadline.
func TestTurnDeadline(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	_, before, _ := h.client.Messages(h.ctx(), h.ep, ws, sid, 0)

	h.rawPost("/core/v1/workspaces/"+ws+"/sessions/"+sid+"/input", map[string]any{"text": "slow", "deadlineMs": time.Now().Add(300 * time.Millisecond).UnixMilli()})
	ts := h.rawPoll(ws, sid)
	if !ts.Failed || ts.FailCode != failDeadline || ts.FailMessage != "ran out of time after less than a minute" {
		t.Fatalf("status = %+v", ts)
	}
	if msgs := h.messages(ws, sid, before); len(msgs) != 0 {
		t.Fatalf("the timed-out turn served messages %q", texts(msgs))
	}

	h.rawPost("/core/v1/workspaces/"+ws+"/sessions/"+sid+"/input", map[string]any{"text": "hello", "deadlineMs": time.Now().Add(time.Minute).UnixMilli()})
	if ts := h.rawPoll(ws, sid); ts.Failed {
		t.Fatalf("a turn within its deadline: %+v", ts)
	}
	if got := h.lastText(ws, sid); got != "hello world" {
		t.Fatalf("reply = %q", got)
	}

	// A deadline at session create.
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(h.rawPost("/core/v1/workspaces/"+ws+"/sessions", map[string]any{"targetId": "fake", "prompt": "slow", "deadlineMs": time.Now().Add(200 * time.Millisecond).UnixMilli()}), &created)
	if ts := h.rawPoll(ws, created.SessionID); !ts.Failed || ts.FailCode != failDeadline {
		t.Fatalf("create with a deadline: status = %+v", ts)
	}
}

// A running turn reports when it started and its latest steps, oldest
// first: a thought and text coalesce into one step each, a tool call is its
// own step (retitled by its update, named by kind and file when untitled),
// at most eight are kept, and the activity is gone once the turn ends.
func TestTurnActivity(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "hello", core.SessionOptions{})
	h.settle(ws, sid)
	start := time.Now().UnixMilli()
	h.send(ws, sid, "steps", core.SessionOptions{})

	var ts turnStatus
	deadline := time.Now().Add(10 * time.Second)
	for {
		ts = h.pollNow(ws, sid)
		if n := len(ts.Activity); n > 0 && ts.Activity[n-1].Kind == activityText {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("activity never reached the text step: %+v", ts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ts.Idle || ts.StartedAtMs < start {
		t.Fatalf("running turn: idle=%v startedAtMs=%d (sent at %d)", ts.Idle, ts.StartedAtMs, start)
	}
	var got []string
	for _, a := range ts.Activity {
		got = append(got, a.Kind+":"+a.Title)
		if a.AtMs < start {
			t.Fatalf("step %+v older than the turn", a)
		}
	}
	want := []string{"tool:step 4", "tool:step 5", "tool:step 6", "tool:step 7", "tool:step 8", "tool:Run python extract.py on two lines", "tool:read: template.pptx", "text:"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("activity = %q, want %q", got, want)
	}

	if err := h.client.Interrupt(h.ctx(), h.ep, ws, sid); err != nil {
		t.Fatal(err)
	}
	if ts := h.rawPoll(ws, sid); ts.StartedAtMs != 0 || len(ts.Activity) != 0 {
		t.Fatalf("an ended turn still reports activity: %+v", ts)
	}
}

// pollNow reads the turn status once over the raw wire.
func (h *harness) pollNow(ws, sid string) turnStatus {
	h.t.Helper()
	resp, err := h.srv.Client().Get(h.srv.URL + "/core/v1/workspaces/" + ws + "/sessions/" + sid + "/turn")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var ts turnStatus
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&ts) != nil {
		h.t.Fatalf("PollTurn: HTTP %d", resp.StatusCode)
	}
	return ts
}

// activityTitle keeps one line of at most maxActivityTitle characters.
func TestActivityTitle(t *testing.T) {
	if got := activityTitle("a\n  b\tc"); got != "a b c" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("界", 200)
	if got := activityTitle(long); utf8.RuneCountInString(got) != maxActivityTitle {
		t.Fatalf("len = %d", utf8.RuneCountInString(got))
	}
}

// A turn that outgrew the model's context window fails as context_overflow,
// and the next turn runs in a fresh ACP session (same protocol session),
// never resuming the overflowing one.
func TestContextOverflowStartsAFreshSession(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	ws, _ := h.workspace()
	sid := h.session(ws, "fake", "whoami", core.SessionOptions{})
	h.settle(ws, sid)
	_, mark, _ := h.client.Messages(h.ctx(), h.ep, ws, sid, 0)
	first := h.messages(ws, sid, 0)
	if len(first) != 1 || !strings.Contains(first[0].Text, "session=") {
		t.Fatalf("first reply = %q", texts(first))
	}
	before := sessionField(first[0].Text)

	h.send(ws, sid, "overflow", core.SessionOptions{})
	ts := h.rawPoll(ws, sid)
	if !ts.Failed || ts.FailCode != failContextOverflow || ts.FailMessage != contextOverflowMessage {
		t.Fatalf("overflow: status = %+v", ts)
	}
	if msgs := h.messages(ws, sid, mark); len(msgs) != 0 {
		t.Fatalf("overflow served messages %q", texts(msgs))
	}

	_, mark, _ = h.client.Messages(h.ctx(), h.ep, ws, sid, 0)
	h.send(ws, sid, "whoami", core.SessionOptions{})
	if ts := h.settle(ws, sid); ts.Failed {
		t.Fatalf("turn after the overflow: %+v", ts)
	}
	next := h.messages(ws, sid, mark)
	if len(next) != 1 {
		t.Fatalf("reply after the overflow = %q", texts(next))
	}
	if after := sessionField(next[0].Text); after == before || after == "" || strings.Contains(next[0].Text, "resumed=true") {
		t.Fatalf("after the overflow the session is %q (was %q): %s", after, before, next[0].Text)
	}
}

// sessionField reads the fake agent's "session=<id>" from a whoami reply.
func sessionField(text string) string {
	for _, f := range strings.Fields(text) {
		if v, ok := strings.CutPrefix(f, "session="); ok {
			return v
		}
	}
	return ""
}

func TestContextOverflowClassification(t *testing.T) {
	for _, s := range []string{
		"the agent returned an error: litellm.ContextWindowExceededError: BadRequestError",
		"prompt is too long: 230000 tokens > 200000 maximum",
		"The input (1245699 tokens) is longer than the model's context length (327680 tokens).",
		"This model's maximum context length is 128000 tokens",
		"Request has too many tokens",
	} {
		if !contextOverflow(s) {
			t.Errorf("not an overflow: %q", s)
		}
	}
	for _, s := range []string{"provider overloaded", "unexpected status 403 Forbidden: model not allowed", "the agent ended the turn with an error"} {
		if contextOverflow(s) {
			t.Errorf("an overflow: %q", s)
		}
	}
}

// The context numbers are the turn's own: absent until the agent reports both,
// and never carried over from an earlier turn.
func TestTurnMetricsReportContextOnlyWhenBothNumbersCame(t *testing.T) {
	s := &session{}
	if m := s.turnMetrics(acpPromptResponse{}); m["contextTokens"] != nil || m["contextWindow"] != nil {
		t.Fatalf("a turn with no report carries context: %v", m)
	}
	s.ctxUsed = 5000 // a used without its size is not a meter
	if m := s.turnMetrics(acpPromptResponse{}); m["contextTokens"] != nil {
		t.Fatalf("used without size: %v", m)
	}
	s.ctxSize = 20000
	m := s.turnMetrics(acpPromptResponse{})
	if m["contextTokens"] != int64(5000) || m["contextWindow"] != int64(20000) {
		t.Fatalf("metrics = %v", m)
	}
}

// A steer joins the running turn of an agent that takes it; with no turn
// running it consumes nothing, and an agent that does not advertise steering
// or refuses it is "unsupported" (the caller queues).
func TestSteerJoinsTheRunningTurn(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{
		fakeAgentConfig(t, "fake", map[string]string{"ACP_FAKE_STEER": "1"}),
		fakeAgentConfig(t, "plain", nil),
		fakeAgentConfig(t, "refuses", map[string]string{"ACP_FAKE_STEER": "refuse"}),
	}})
	steer := func(name, prompt string, turn bool) (core.SteerOutcome, string, string) {
		ws, _ := h.workspace()
		sid := h.session(ws, name, prompt, core.SessionOptions{})
		if turn {
			time.Sleep(150 * time.Millisecond)
		} else {
			h.settle(ws, sid)
		}
		out, err := h.client.Steer(h.ctx(), h.ep, ws, sid, "use blue instead")
		if err != nil {
			t.Fatalf("%s: Steer: %v", name, err)
		}
		_ = h.client.Interrupt(h.ctx(), h.ep, ws, sid)
		h.settle(ws, sid)
		return out, strings.Join(texts(h.messages(ws, sid, 0)), "|"), sid
	}
	if out, msgs, _ := steer("fake", "steps", true); out != core.SteerInjected || !strings.Contains(msgs, "steered: use blue instead") {
		t.Fatalf("running turn: outcome %q, messages %q", out, msgs)
	}
	if out, msgs, _ := steer("fake", "two", false); out != core.SteerIdle || strings.Contains(msgs, "steered") {
		t.Fatalf("idle session: outcome %q, messages %q", out, msgs)
	}
	if out, _, _ := steer("plain", "steps", true); out != core.SteerUnsupported {
		t.Fatalf("agent without steering: outcome %q", out)
	}
	if out, _, _ := steer("refuses", "steps", true); out != core.SteerUnsupported {
		t.Fatalf("agent that refuses: outcome %q", out)
	}
	ws, _ := h.workspace()
	if _, err := h.client.Steer(h.ctx(), h.ep, ws, "nope", "x"); err == nil {
		t.Fatal("a steer to no session must fail")
	}
}

func TestSteerIsAdvertised(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	d, err := h.client.Discovery(h.ctx(), h.ep)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.Capabilities, protocol.CapabilitySteer) {
		t.Fatalf("capabilities = %v", d.Capabilities)
	}
}
