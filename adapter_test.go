package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	home := t.TempDir()
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
		if idle, err := h.client.SessionIdle(h.ctx(), h.ep, ws, sid); err == nil && idle {
			ts, err := h.client.PollTurn(h.ctx(), h.ep, ws, sid)
			if err != nil {
				h.t.Fatalf("PollTurn: %v", err)
			}
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
	latest, err := h.client.LatestSession(h.ctx(), h.ep, ws)
	if err != nil || latest != sid {
		t.Fatalf("LatestSession = %q, %v", latest, err)
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
	if idle, _ := h.client.SessionIdle(h.ctx(), h.ep, ws, sid); idle {
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
	deadline := time.Now().Add(20 * time.Second)
	for {
		if idle, err := h.client.SessionIdle(h.ctx(), h.ep, ws, sid); err == nil && idle {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatal("session never settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	var perr *protocol.ProtocolError
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, "README.md", "x"); !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
		t.Fatalf("write outside .room = %v", err)
	}
	if err := h.client.WriteWorkspaceFile(h.ctx(), h.ep, ws, ".room/../../x", "x"); !errors.As(err, &perr) || perr.Code != protocol.ErrorBadRequest {
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

func TestSandboxExecutionIsRefused(t *testing.T) {
	h := newHarness(t, Config{Agents: []AgentConfig{fakeAgentConfig(t, "fake", nil)}})
	if core.Supports(h.ctx(), h.client, h.ep, core.CapabilitySandboxExec) {
		t.Fatal("sandboxExec must not be advertised")
	}
	ws, _ := h.workspace()
	body := `{"prompt":"hi","execution":{"mode":"sandbox","sandbox":{"url":"http://127.0.0.1:1","execToken":"t","machine":"m","space":"s","workdir":"/tmp"}}}`
	resp, err := h.srv.Client().Post(h.srv.URL+protocol.WorkspaceSessionsPath(ws), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sandboxed create = %d, want 400", resp.StatusCode)
	}
	resp, err = h.srv.Client().Post(h.srv.URL+protocol.PathWorkspaces, "application/json", strings.NewReader(`{"workdir":"/tmp/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("workdir workspace = %d, want 400", resp.StatusCode)
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
