package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// maxSessionMessages bounds the completed messages a session keeps; the
// bridge reads them by a monotonic cursor shortly after they complete.
const maxSessionMessages = 1000

// maxReadTextBytes caps one fs/read_text_file answer.
const maxReadTextBytes = 16 << 20

// errSessionLost: the session's agent process is gone and the agent can
// neither resume nor load sessions, so the conversation cannot continue.
var errSessionLost = errors.New("the agent process exited and the agent cannot resume sessions")

// errSessionOption: the agent refused the configured mode or the sender's
// model / reasoning choice (a bad request, not an unavailable core).
var errSessionOption = errors.New("session option rejected")

type turnInput struct {
	text string
	opts protocol.SessionOptions
}

// session is one Core Protocol session: an ACP session in its own agent
// process, running one prompt turn at a time.
type session struct {
	id          string
	workspaceID string
	ws          workspaceRoot
	agent       AgentConfig
	// profile is the role the session wears (nil = none), kept in memory so
	// a continued session re-applies it.
	profile   *core.AgentProfile
	pol       policy
	reg       *registry
	createdAt time.Time
	ctx       context.Context
	cancelCtx context.CancelFunc

	mu        sync.Mutex
	proc      *agentProc
	acpID     string
	resumable bool // the agent advertises session/resume
	loadable  bool // the agent advertises loadSession
	options   []acpConfigOption
	// spawnModel is the model a config-declared catalog applied through
	// modelArgs at spawn ("" otherwise).
	spawnModel string
	loading    bool // a session/load replay is in progress: drop its updates

	queue     []turnInput
	running   bool
	cancelled bool
	status    turnStatus
	// statusServed: the current failed status was returned by a poll; a lost
	// session answers session_not_found once its failure has been seen.
	statusServed bool
	lost         bool
	closed       bool
	lastActive   time.Time

	msgs     []protocol.Message
	version  int64
	text     strings.Builder
	textID   string
	thinking strings.Builder
	cost     *costState
	turnCost float64
	// refused lists the tool calls the policy refused during the turn, so a
	// turn that ends without a reply says why.
	refused []string

	terminals map[string]*terminal
}

type costState struct {
	amount   float64
	currency string
}

func newSession(id, workspaceID string, ws workspaceRoot, a AgentConfig, reg *registry) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{id: id, workspaceID: workspaceID, ws: ws, agent: a, pol: newPolicy(a.Permissions), reg: reg,
		createdAt: time.Now(), ctx: ctx, cancelCtx: cancel, lastActive: time.Now(), terminals: map[string]*terminal{}}
}

// open starts the agent and its ACP session, applies the configured mode and
// the sender's model/reasoning, and queues the first prompt.
func (s *session) open(ctx context.Context, prompt string, opts protocol.SessionOptions) error {
	if s.reg.usesConfigModels(s.agent) {
		s.spawnModel = opts.Model
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	pc := s.agent.effectiveProfileConfig()
	if err := materializeProfile(s.profile, s.ws, pc); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	proc, err := startAgent(hctx, s.agent, s.agent.spawnArgs(s.spawnModel), s.ws.dir, s, profileEnviron(s.profile, pc)...)
	if err != nil {
		return err
	}
	servers, err := profileMCPServers(s.profile, s.agent, proc.init)
	if err != nil {
		proc.stop()
		return err
	}
	var resp acpSessionResponse
	if err := proc.call(hctx, methodSessionNew, acpNewSessionRequest{Cwd: s.ws.dir, McpServers: servers, Meta: profileMeta(s.profile, pc)}, &resp); err != nil {
		err = proc.diagnose(err)
		proc.stop()
		return fmt.Errorf("session/new: %w", err)
	}
	if resp.SessionID == "" {
		proc.stop()
		return errors.New("session/new returned no session id")
	}
	s.mu.Lock()
	s.proc, s.acpID = proc, resp.SessionID
	s.resumable, s.loadable = proc.init.canResume(), proc.init.AgentCapabilities.LoadSession
	s.options = resp.ConfigOptions
	s.mu.Unlock()
	go s.watch(proc)
	s.reg.observe(s.agent.Name, proc.init, resp.ConfigOptions)
	if err := s.applyMode(hctx, proc, resp.Modes); err != nil {
		s.close()
		return fmt.Errorf("%w: %v", errSessionOption, err)
	}
	if err := s.applyOptions(hctx, proc, opts); err != nil {
		s.close()
		return fmt.Errorf("%w: %v", errSessionOption, err)
	}
	return s.enqueue(turnInput{text: prompt, opts: opts})
}

// applyMode sets the agent's configured session mode: through a "mode"
// config option when the agent has one, else session/set_mode.
func (s *session) applyMode(ctx context.Context, proc *agentProc, modes *acpModeState) error {
	if s.agent.Mode == "" {
		return nil
	}
	if opt, ok := s.option(configCategoryMode); ok {
		return s.setOption(ctx, proc, opt, s.agent.Mode)
	}
	if modes != nil {
		if modes.CurrentModeID == s.agent.Mode {
			return nil
		}
		for _, m := range modes.AvailableModes {
			if m.ID == s.agent.Mode {
				return proc.call(ctx, methodSessionSetMode, acpSetModeRequest{SessionID: s.acpID, ModeID: s.agent.Mode}, nil)
			}
		}
	}
	return fmt.Errorf("agent %s offers no session mode %q", s.agent.Name, s.agent.Mode)
}

// applyOptions applies the sender's model and reasoning level through the
// agent's model / thought_level config options when they differ from the
// session's current values. An agent without a reasoning selector ignores
// the reasoning choice (the picker never offered one); a model chosen from a
// config-declared catalog was applied at spawn.
func (s *session) applyOptions(ctx context.Context, proc *agentProc, opts protocol.SessionOptions) error {
	if opts.Model != "" {
		if opt, ok := s.option(configCategoryModel); ok {
			if opt.current() != opts.Model {
				if err := s.setOption(ctx, proc, opt, opts.Model); err != nil {
					return err
				}
			}
		} else if len(s.agent.Models) > 0 && s.spawnModel != opts.Model {
			// A config-declared catalog applies its model at spawn only.
			slog.Warn("bnbloc-acp model change needs a new session; keeping the session's model",
				"agent", s.agent.Name, "session", s.id, "model", s.spawnModel, "requested", opts.Model)
		}
	}
	if opts.ReasoningEffort != "" {
		if opt, ok := s.option(configCategoryThought); ok && opt.current() != opts.ReasoningEffort {
			if err := s.setOption(ctx, proc, opt, opts.ReasoningEffort); err != nil {
				return err
			}
		}
	}
	return nil
}

// option finds the session's config option of a category (or, for an agent
// that sets no category, the option whose id is the category name).
func (s *session) option(category string) (acpConfigOption, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return findOption(s.options, category)
}

func findOption(opts []acpConfigOption, category string) (acpConfigOption, bool) {
	for _, o := range opts {
		if o.Category == category {
			return o, true
		}
	}
	for _, o := range opts {
		if o.Category == "" && o.ID == category {
			return o, true
		}
	}
	return acpConfigOption{}, false
}

func (s *session) setOption(ctx context.Context, proc *agentProc, opt acpConfigOption, value string) error {
	var resp acpSetConfigOptionResponse
	if err := proc.call(ctx, methodSessionSetValue, acpSetConfigOptionRequest{SessionID: s.acpID, ConfigID: opt.ID, Value: value}, &resp); err != nil {
		return fmt.Errorf("set %s to %q: %w", opt.ID, value, err)
	}
	if len(resp.ConfigOptions) > 0 {
		s.mu.Lock()
		s.options = resp.ConfigOptions
		s.mu.Unlock()
	}
	return nil
}

// enqueue queues one prompt; the session runs its turns one at a time.
func (s *session) enqueue(in turnInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost || s.closed {
		return errSessionLost
	}
	s.queue = append(s.queue, in)
	if !s.running {
		s.running = true
		go s.run()
	}
	return nil
}

func (s *session) run() {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 || s.closed {
			s.running = false
			s.queue = nil
			s.lastActive = time.Now()
			s.mu.Unlock()
			return
		}
		in := s.queue[0]
		s.queue = s.queue[1:]
		s.cancelled = false
		s.status, s.statusServed = turnStatus{}, false
		s.turnCost, s.refused = 0, nil
		if s.cost != nil {
			s.turnCost = s.cost.amount
		}
		s.mu.Unlock()
		s.turn(in)
	}
}

// turn runs one prompt to its end and records how it ended.
func (s *session) turn(in turnInput) {
	first := s.messageCount()
	proc, err := s.ensureProc()
	if err == nil {
		hctx, cancel := context.WithTimeout(s.ctx, handshakeTimeout)
		err = s.applyOptions(hctx, proc, in.opts)
		cancel()
	}
	var resp acpPromptResponse
	if err == nil && !s.isCancelled() {
		err = proc.call(s.ctx, methodSessionPrompt, acpPromptRequest{SessionID: s.acpID, Prompt: []acpContentBlock{{Type: "text", Text: in.text}}}, &resp)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cancelled := s.cancelled || resp.StopReason == stopReasonCancelled
	if !cancelled {
		if code, msg := turnFailure(err, resp); code != "" {
			// An errored turn is a failed turn, never a reply: the text the
			// agent streamed since its last completed message is the failure's
			// output, so it is dropped rather than completed (a poll never sees
			// it as a reply, whatever order the caller reads status and
			// messages in).
			s.discardText()
			s.setFailed(code, msg)
			slog.Warn("bnbloc-acp turn failed", "agent", s.agent.Name, "session", s.id, "code", code, "error", err, "stopReason", resp.StopReason)
			return
		}
	}
	if s.finalizeText() {
		s.msgs[len(s.msgs)-1].Metrics = s.turnMetrics(resp)
	}
	replied := s.version > first
	switch {
	case cancelled:
		// An interrupted turn is not a failure.
	case resp.StopReason == stopReasonRefusal && !replied:
		s.setFailed(failRefusal, "the agent refused to continue")
	case (resp.StopReason == stopReasonMaxTokens || resp.StopReason == stopReasonMaxTurnRequest) && !replied:
		s.setFailed(failNoReply, "the agent stopped before replying ("+resp.StopReason+")")
	case len(s.refused) > 0 && !replied:
		s.setFailed(failPermissionRefused, "the agent stopped after this core's permission policy refused: "+strings.Join(s.refused, "; "))
	}
}

// turnStatus is the PollTurn answer: Core Protocol v1's PollTurnResponse plus
// failCode, a stable machine-readable cause of a failed turn (an additive
// field; failMessage stays the room-visible reason).
type turnStatus struct {
	protocol.PollTurnResponse
	FailCode string `json:"failCode,omitempty"`
}

// Fail codes of a failed turn.
const (
	failAgentError        = "agent_error"        // a JSON-RPC error, an error stop reason, a failed respawn
	failAgentExited       = "agent_exited"       // the agent process exited during the turn
	failSessionLost       = "session_lost"       // the agent can neither resume nor load the session
	failRefusal           = "refusal"            // stop reason refusal, no reply
	failNoReply           = "no_reply"           // max_tokens / max_turn_requests, no reply
	failPermissionRefused = "permission_refused" // the policy refused, no reply
)

func (s *session) setFailed(code, msg string) {
	s.status = turnStatus{PollTurnResponse: protocol.PollTurnResponse{Failed: true, FailMessage: msg}, FailCode: code}
}

// turnFailure is how an errored turn failed: its fail code and room-visible
// reason (the agent's own JSON-RPC error message, never its stderr), or ""
// when the turn did not error.
func turnFailure(err error, resp acpPromptResponse) (code, msg string) {
	var rerr *rpcError
	switch {
	case err == nil && resp.StopReason == stopReasonError:
		return failAgentError, "the agent ended the turn with an error"
	case err == nil:
		return "", ""
	case errors.As(err, &rerr):
		return failAgentError, "the agent returned an error: " + rerr.Message
	case errors.Is(err, errAgentExited):
		return failAgentExited, "the agent process exited during the turn"
	case errors.Is(err, errSessionLost):
		return failSessionLost, errSessionLost.Error()
	default:
		return failAgentError, err.Error()
	}
}

func (s *session) isCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

func (s *session) messageCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// turnMetrics is the turn's usage on its last message: the model in force,
// per-turn tokens when the agent reported them, the turn's USD cost when the
// agent reports a cumulative session cost.
func (s *session) turnMetrics(resp acpPromptResponse) map[string]any {
	m := map[string]any{}
	if opt, ok := findOption(s.options, configCategoryModel); ok && opt.current() != "" {
		m["model"] = opt.current()
	} else if s.spawnModel != "" {
		m["model"] = s.spawnModel
	}
	if u := resp.Usage; u != nil {
		m["inputTokens"], m["outputTokens"] = u.InputTokens, u.OutputTokens
	} else {
		m["reported"] = false
	}
	if c := s.cost; c != nil && strings.EqualFold(c.currency, "USD") && c.amount >= s.turnCost {
		m["costUsd"] = c.amount - s.turnCost
	}
	return m
}

// ensureProc returns the running agent, respawning a stopped one and
// continuing its ACP session (session/resume, else session/load with the
// replay discarded).
func (s *session) ensureProc() (*agentProc, error) {
	s.mu.Lock()
	proc := s.proc
	resumable, loadable, closed := s.resumable, s.loadable, s.closed
	s.mu.Unlock()
	if closed {
		return nil, errSessionLost
	}
	if proc != nil && proc.alive() {
		return proc, nil
	}
	if !resumable && !loadable {
		s.markLost()
		return nil, errSessionLost
	}
	ctx, cancel := context.WithTimeout(s.ctx, handshakeTimeout)
	defer cancel()
	// The continued session wears the same profile: its files are written
	// again (the workspace may have changed meanwhile) and its env, MCP
	// servers and system-prompt append go to the new process.
	pc := s.agent.effectiveProfileConfig()
	if err := materializeProfile(s.profile, s.ws, pc); err != nil {
		s.markLost()
		return nil, fmt.Errorf("profile: %w", err)
	}
	proc, err := startAgent(ctx, s.agent, s.agent.spawnArgs(s.spawnModel), s.ws.dir, s, profileEnviron(s.profile, pc)...)
	if err != nil {
		s.markLost()
		return nil, err
	}
	servers, err := profileMCPServers(s.profile, s.agent, proc.init)
	if err != nil {
		proc.stop()
		s.markLost()
		return nil, err
	}
	req := acpContinueSessionRequest{SessionID: s.acpID, Cwd: s.ws.dir, McpServers: servers, Meta: profileMeta(s.profile, pc)}
	var resp acpSessionResponse
	if resumable {
		err = proc.call(ctx, methodSessionResume, req, &resp)
	} else {
		s.setLoading(true)
		err = proc.call(ctx, methodSessionLoad, req, &resp)
		s.setLoading(false)
	}
	if err != nil {
		proc.stop()
		s.markLost()
		return nil, fmt.Errorf("continue session: %w", err)
	}
	s.mu.Lock()
	s.proc = proc
	if len(resp.ConfigOptions) > 0 {
		s.options = resp.ConfigOptions
	}
	s.mu.Unlock()
	go s.watch(proc)
	slog.Info("bnbloc-acp session continued in a new agent process", "agent", s.agent.Name, "session", s.id, "resumed", resumable)
	return proc, nil
}

func (s *session) setLoading(v bool) {
	s.mu.Lock()
	s.loading = v
	s.mu.Unlock()
}

func (s *session) markLost() {
	s.mu.Lock()
	s.lost = true
	s.mu.Unlock()
}

// watch notices the agent process ending: its terminals die with it, and a
// session that cannot continue is lost.
func (s *session) watch(proc *agentProc) {
	<-proc.exited
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != proc {
		return
	}
	s.proc = nil
	s.killTerminalsLocked()
	if !s.closed && !s.resumable && !s.loadable {
		s.lost = true
	}
}

// reapIfIdle stops the agent of a session idle longer than d. Only a session
// the agent can continue is reaped; another keeps its process (stopping it
// would end the conversation).
func (s *session) reapIfIdle(d time.Duration) {
	s.mu.Lock()
	proc := s.proc
	idle := !s.running && len(s.queue) == 0 && time.Since(s.lastActive) > d
	if proc == nil || !idle || !(s.resumable || s.loadable) {
		s.mu.Unlock()
		return
	}
	s.proc = nil
	s.killTerminalsLocked()
	s.mu.Unlock()
	slog.Info("bnbloc-acp stopping idle agent", "agent", s.agent.Name, "session", s.id)
	proc.stop()
}

// interrupt cancels the running turn (session/cancel) and drops queued
// input. Idempotent; no error without a running turn.
func (s *session) interrupt() {
	s.mu.Lock()
	s.queue = nil
	proc := s.proc
	running := s.running
	if running {
		s.cancelled = true
	}
	acpID := s.acpID
	s.mu.Unlock()
	if running && proc != nil {
		_ = proc.conn.Notify(methodSessionCancel, acpCancelNotification{SessionID: acpID})
	}
}

func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	s.queue = nil
	proc := s.proc
	s.proc = nil
	s.killTerminalsLocked()
	s.mu.Unlock()
	s.cancelCtx()
	if proc != nil {
		proc.stop()
	}
}

// idle: no turn running and none queued.
func (s *session) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running && len(s.queue) == 0
}

// poll is the turn status. A lost session answers it until its failure has
// been seen once, then reports the session gone so the caller rebinds.
func (s *session) poll() (turnStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	busy := s.running || len(s.queue) > 0
	if s.lost && !busy && (!s.status.Failed || s.statusServed) {
		return turnStatus{}, false
	}
	st := s.status
	st.Idle = !busy
	if st.Idle && st.Failed {
		s.statusServed = true
	}
	return st, true
}

func (s *session) messages(after int64) ([]protocol.Message, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []protocol.Message{}
	for _, m := range s.msgs {
		if m.Version > after {
			out = append(out, m)
		}
	}
	return out, s.version
}

// --- what the agent sends the client ---

// Notify handles session/update: assistant text aggregates into completed
// messages (see finalizeText), thoughts ride the next message, config and
// cost updates are recorded. Runs on the connection's reader goroutine.
func (s *session) Notify(method string, params json.RawMessage) {
	if method != methodSessionUpdate {
		return
	}
	var n acpSessionNotification
	var u acpSessionUpdate
	if json.Unmarshal(params, &n) != nil || json.Unmarshal(n.Update, &u) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.SessionUpdate == "config_option_update" {
		s.options = u.ConfigOptions
		return
	}
	if s.loading {
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if u.Content == nil || u.Content.Type != "text" {
			return
		}
		if u.MessageID != "" && s.textID != "" && u.MessageID != s.textID {
			s.finalizeText()
		}
		if u.MessageID != "" {
			s.textID = u.MessageID
		}
		s.text.WriteString(u.Content.Text)
	case "agent_thought_chunk":
		if u.Content != nil && u.Content.Type == "text" {
			s.finalizeText()
			s.thinking.WriteString(u.Content.Text)
		}
	case "tool_call", "tool_call_update", "plan":
		// The agent moved on: the text so far is a complete message.
		s.finalizeText()
	case "usage_update":
		if u.Cost != nil {
			s.cost = &costState{amount: u.Cost.Amount, currency: u.Cost.Currency}
		}
	}
}

// discardText drops the message being aggregated (and its thinking) without
// completing it.
func (s *session) discardText() {
	if n := s.text.Len(); n > 0 {
		slog.Info("bnbloc-acp dropping the text of a failed turn", "agent", s.agent.Name, "session", s.id, "bytes", n)
	}
	s.text.Reset()
	s.textID = ""
	s.thinking.Reset()
}

// finalizeText completes the message being aggregated (whitespace-only text
// is dropped) and reports whether it produced one.
func (s *session) finalizeText() bool {
	text := s.text.String()
	id := s.textID
	s.text.Reset()
	s.textID = ""
	if strings.TrimSpace(text) == "" {
		return false
	}
	s.version++
	if id == "" {
		id = fmt.Sprintf("%s-%d", s.id, s.version)
	}
	s.msgs = append(s.msgs, protocol.Message{
		Version: s.version, Seq: s.version, Role: "assistant", Text: text, UserVisible: true, Completed: true,
		MessageID: id, CreatedAtMs: time.Now().UnixMilli(), Thinking: s.thinking.String(),
	})
	s.thinking.Reset()
	if over := len(s.msgs) - maxSessionMessages; over > 0 {
		s.msgs = append([]protocol.Message(nil), s.msgs[over:]...)
	}
	return true
}

// Request answers the agent's client requests by policy (policy.go).
func (s *session) Request(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case methodRequestPermission:
		var req acpRequestPermissionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		outcome, why := s.pol.decidePermission(s.ws, req)
		if why != "" {
			s.noteRefusal(fmt.Sprintf("%s (%s)", truncate(req.ToolCall.Title, 80), why))
		}
		slog.Info("bnbloc-acp permission", "agent", s.agent.Name, "session", s.id, "kind", req.ToolCall.Kind,
			"tool", truncate(req.ToolCall.Title, 120), "outcome", outcome.Outcome, "option", outcome.OptionID, "reason", why)
		return acpRequestPermissionResponse{Outcome: outcome}, nil
	case methodFSReadTextFile:
		var req acpReadTextFileRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return s.readTextFile(req)
	case methodFSWriteTextFile:
		var req acpWriteTextFileRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return s.writeTextFile(req)
	case methodTerminalCreate:
		var req acpCreateTerminalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return s.createTerminal(req)
	case methodTerminalOutput, methodTerminalWaitExit, methodTerminalKill, methodTerminalRelease:
		var req acpTerminalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return s.terminalOp(ctx, method, req.TerminalID)
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: "method not supported by bnbloc-acp: " + method}
}

// noteRefusal records a request the policy refused during the turn.
func (s *session) noteRefusal(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refused) < 5 {
		s.refused = append(s.refused, what)
	}
}

func invalidParams(err error) *rpcError {
	return &rpcError{Code: rpcInvalidParams, Message: err.Error()}
}

func denied(what string) *rpcError {
	return &rpcError{Code: rpcInvalidParams, Message: what + " is not allowed by the bnbloc-acp permission policy"}
}

func (s *session) readTextFile(req acpReadTextFileRequest) (any, *rpcError) {
	if !s.pol.allows("read") {
		s.noteRefusal("reading " + req.Path)
		return nil, denied("reading files")
	}
	path, err := s.ws.resolve(req.Path)
	if err != nil {
		return nil, invalidParams(err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, &rpcError{Code: -32002, Message: err.Error()}
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > maxReadTextBytes {
		return nil, invalidParams(fmt.Errorf("%s is not a regular file under %d bytes", req.Path, maxReadTextBytes))
	}
	if req.Line == nil && req.Limit == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, invalidParams(err)
		}
		return acpReadTextFileResponse{Content: string(raw)}, nil
	}
	start := 1
	if req.Line != nil && *req.Line > 0 {
		start = *req.Line
	}
	limit := -1
	if req.Limit != nil {
		limit = *req.Limit
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxReadTextBytes)
	for n := 1; sc.Scan(); n++ {
		if n < start {
			continue
		}
		if limit >= 0 && n >= start+limit {
			break
		}
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return acpReadTextFileResponse{Content: b.String()}, nil
}

func (s *session) writeTextFile(req acpWriteTextFileRequest) (any, *rpcError) {
	if !s.pol.allows("edit") {
		s.noteRefusal("writing " + req.Path)
		return nil, denied("writing files")
	}
	path, err := s.ws.resolveWritable(req.Path)
	if err != nil {
		return nil, invalidParams(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, invalidParams(err)
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
		return nil, invalidParams(err)
	}
	return struct{}{}, nil
}

func (s *session) createTerminal(req acpCreateTerminalRequest) (any, *rpcError) {
	if !s.pol.allows("execute") {
		s.noteRefusal("running " + truncate(req.Command, 80))
		return nil, denied("running commands")
	}
	dir := s.ws.dir
	if req.Cwd != nil && *req.Cwd != "" {
		real, err := s.ws.resolve(*req.Cwd)
		if err != nil {
			return nil, invalidParams(err)
		}
		dir = real
	}
	var env []string
	for _, e := range req.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	t, err := startTerminal(req, dir, s.agent.environ(env...))
	if err != nil {
		return nil, invalidParams(err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		t.kill()
		return nil, invalidParams(errors.New("session closed"))
	}
	s.terminals[t.id] = t
	s.mu.Unlock()
	slog.Info("bnbloc-acp terminal", "agent", s.agent.Name, "session", s.id, "terminal", t.id, "command", truncate(req.Command, 120))
	return acpCreateTerminalResponse{TerminalID: t.id}, nil
}

func (s *session) terminalOp(ctx context.Context, method, id string) (any, *rpcError) {
	s.mu.Lock()
	t := s.terminals[id]
	if method == methodTerminalRelease {
		delete(s.terminals, id)
	}
	s.mu.Unlock()
	if t == nil {
		return nil, &rpcError{Code: -32002, Message: "unknown terminal " + id}
	}
	switch method {
	case methodTerminalOutput:
		return t.output(), nil
	case methodTerminalWaitExit:
		st, err := t.wait(ctx)
		if err != nil {
			return nil, &rpcError{Code: rpcInternalError, Message: err.Error()}
		}
		return st, nil
	default: // kill, release
		t.kill()
		return struct{}{}, nil
	}
}

func (s *session) killTerminalsLocked() {
	for id, t := range s.terminals {
		go t.kill()
		delete(s.terminals, id)
	}
}
