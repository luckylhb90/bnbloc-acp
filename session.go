package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// errSandboxChanged: a sandboxed input changed what the agent process was started
// with and the agent cannot resume or load sessions, so it cannot be continued
// in a new process under the new descriptor.
var errSandboxChanged = fmt.Errorf("%w: the sandbox changed and the agent cannot continue its session in a new process", errSandboxRefused)

// errSessionOption: the agent refused the configured mode or the sender's
// model / reasoning choice (a bad request, not an unavailable core).
var errSessionOption = errors.New("session option rejected")

type turnInput struct {
	text string
	opts protocol.SessionOptions
	// deadline is when the turn must end (zero = none given; live.go).
	deadline time.Time
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
	// fresh: the conversation outgrew the model's context; the next turn
	// starts a new ACP session (overflow.go).
	fresh    bool
	loadable bool // the agent advertises loadSession
	options  []acpConfigOption
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
	// ctxUsed / ctxSize are the context the agent last reported this turn (a
	// usage_update's used and size; 0 = not reported this turn).
	ctxUsed, ctxSize int64
	// refused lists the tool calls the policy refused during the turn, so a
	// turn that ends without a reply says why.
	refused []string
	// metaErr is the turn failure an agent reported in session_info_update
	// metadata ("" = none), and metaLastErr the last error it reported there
	// (its cause; metaTurnState).
	metaErr, metaLastErr string
	// startedAt and activity are the running turn's start and latest steps
	// (live.go); deadlineHit marks a turn cancelled at its deadline.
	startedAt   time.Time
	activity    []turnActivity
	deadlineHit bool
	// mcpDrops are the profile MCP servers the current ACP session could not
	// be given, and mcpTold the last set the agent was told of (mcpnote.go).
	mcpDrops []mcpDrop
	mcpTold  string

	terminals map[string]*terminal

	// Sandbox (sandbox.go): sandboxed marks a session that runs its agent
	// confined, for good (a sandboxed session never runs on the host); exe is
	// the latest descriptor an input carried (its token is also in the token
	// file); procPin is the pin the running process was started with.
	sandboxed bool
	exe       core.SandboxExecution
	procPin   string
	sbHost    sandboxHost
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
func (s *session) open(ctx context.Context, first turnInput) error {
	opts := first.opts
	if s.reg.usesConfigModels(s.agent) {
		s.spawnModel = opts.Model
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	pc := s.agent.effectiveProfileConfig()
	if err := materializeProfile(s.profile, s.ws, pc); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	launch, pin, err := s.launch(ctx)
	if err != nil {
		return err
	}
	proc, err := startAgentIn(hctx, s.agent, s.agent.spawnArgs(s.spawnModel), s.ws.dir, launch, s, profileEnviron(s.profile, pc)...)
	if err != nil {
		return err
	}
	servers, err := s.sessionMCPServers(proc, pc)
	if err != nil {
		proc.stop()
		return err
	}
	var resp acpSessionResponse
	if err := proc.call(hctx, methodSessionNew, acpNewSessionRequest{Cwd: s.ws.dir, McpServers: servers, Meta: profileMeta(s.profile, pc)}, &resp); err != nil {
		err = proc.diagnose(err)
		// A guest agent's refusal is explained by what it was started with: the names of the
		// variables of its environment go with the error (never their values).
		if names := launch.guestEnvNames(ctx); len(names) > 0 {
			err = fmt.Errorf("%w [the agent's environment: %s]", err, strings.Join(names, " "))
		}
		launch.distrust()
		proc.stop()
		return fmt.Errorf("session/new: %w", err)
	}
	if resp.SessionID == "" {
		proc.stop()
		return errors.New("session/new returned no session id")
	}
	s.mu.Lock()
	s.proc, s.acpID, s.procPin = proc, resp.SessionID, pin
	s.resumable, s.loadable = proc.init.canResume(), proc.init.AgentCapabilities.LoadSession
	s.options = resp.normalizeOptions()
	s.mu.Unlock()
	go s.watch(proc)
	s.reg.observe(s.agent.Name, proc.init, s.options)
	if err := s.applyMode(hctx, proc, resp.Modes); err != nil {
		s.close()
		return fmt.Errorf("%w: %v", errSessionOption, err)
	}
	if err := s.applyOptions(hctx, proc, opts); err != nil {
		s.close()
		return fmt.Errorf("%w: %v", errSessionOption, err)
	}
	return s.enqueue(first)
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
	if opt.legacy {
		// An agent that reports its models the earlier way switches them the earlier
		// way. The option is remade with the new current value; the agent's answer
		// carries nothing to read it from.
		if err := proc.call(ctx, methodSessionSetModel, acpSetModelRequest{SessionID: s.acpID, ModelID: value}, nil); err != nil {
			return fmt.Errorf("set model to %q: %w", value, err)
		}
		s.mu.Lock()
		for i := range s.options {
			if s.options[i].legacy {
				s.options[i].CurrentValue, _ = json.Marshal(value)
			}
		}
		s.mu.Unlock()
		return nil
	}
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
		s.turnCost, s.refused, s.metaErr, s.metaLastErr = 0, nil, "", ""
		// A number from an earlier turn is not this turn's: an agent that stops
		// reporting must not keep showing its last one.
		s.ctxUsed, s.ctxSize = 0, 0
		s.startedAt, s.activity, s.deadlineHit = time.Now(), nil, false
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
	if !in.deadline.IsZero() {
		stop := s.watchDeadline(in.deadline)
		defer stop()
	}
	proc, err := s.ensureProc()
	if err == nil {
		hctx, cancel := context.WithTimeout(s.ctx, handshakeTimeout)
		err = s.applyOptions(hctx, proc, in.opts)
		cancel()
	}
	var resp acpPromptResponse
	if err == nil && !s.isCancelled() {
		prompt := []acpContentBlock{{Type: "text", Text: in.text}}
		if note := s.takeMCPNote(); note != "" {
			prompt = append([]acpContentBlock{{Type: "text", Text: note}}, prompt...)
		}
		err = proc.call(s.ctx, methodSessionPrompt, acpPromptRequest{SessionID: s.acpID, Prompt: prompt}, &resp)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadlineHit {
		// Out of time: what the agent was still writing is dropped, like an
		// errored turn's (the messages it completed before stay).
		s.discardText()
		s.setFailed(failDeadline, deadlineMessage(s.startedAt, in.deadline))
		slog.Warn("bnbloc-acp turn ran out of time", "agent", s.agent.Name, "session", s.id, "deadline", in.deadline)
		return
	}
	cancelled := s.cancelled || resp.StopReason == stopReasonCancelled
	if !cancelled {
		if code, msg := turnFailure(err, resp, s.metaErr); code != "" {
			if code != failSessionLost && contextOverflow(msg) {
				// The conversation outgrew the model: the next turn starts a
				// fresh ACP session rather than resuming this one.
				code, msg, s.fresh = failContextOverflow, contextOverflowMessage, true
			}
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
// failCode, a stable machine-readable cause of a failed turn (failMessage
// stays the room-visible reason), and a running turn's start and activity.
type turnStatus struct {
	protocol.PollTurnResponse
	FailCode string `json:"failCode,omitempty"`
	// StartedAtMs and Activity describe a running turn (live.go).
	StartedAtMs int64          `json:"startedAtMs,omitempty"`
	Activity    []turnActivity `json:"activity,omitempty"`
}

// Fail codes of a failed turn.
const (
	failAgentError        = "agent_error"        // a JSON-RPC error, an error stop reason, a failed respawn
	failAgentExited       = "agent_exited"       // the agent process exited during the turn
	failSessionLost       = "session_lost"       // the agent can neither resume nor load the session
	failRefusal           = "refusal"            // stop reason refusal, no reply
	failNoReply           = "no_reply"           // max_tokens / max_turn_requests, no reply
	failPermissionRefused = "permission_refused" // the policy refused, no reply
	failDeadline          = "deadline"           // still running at its deadline (live.go)
	// failContextOverflow (overflow.go): the conversation outgrew the
	// model's context window.
)

func (s *session) setFailed(code, msg string) {
	s.status = turnStatus{PollTurnResponse: protocol.PollTurnResponse{Failed: true, FailMessage: msg}, FailCode: code}
}

// turnFailure is how an errored turn failed: its fail code and room-visible
// reason (the agent's own JSON-RPC error message or the error its metadata
// reported, never its stderr), or "" when the turn did not error.
func turnFailure(err error, resp acpPromptResponse, metaErr string) (code, msg string) {
	var rerr *rpcError
	switch {
	case err == nil && resp.StopReason == stopReasonError:
		return failAgentError, "the agent ended the turn with an error"
	case err == nil && metaErr != "":
		return failAgentError, "the agent returned an error: " + metaErr
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
// agent reports a cumulative session cost, and the context it holds
// (contextTokens of contextWindow) when it reports a usage_update.
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
	// How full the conversation is after this turn's last request (the agent's
	// own count and window; the client shows them as a meter).
	if s.ctxUsed > 0 && s.ctxSize > 0 {
		m["contextTokens"], m["contextWindow"] = s.ctxUsed, s.ctxSize
	}
	return m
}

// ensureProc returns the running agent, respawning a stopped one and
// continuing its ACP session (session/resume, else session/load with the
// replay discarded).
func (s *session) ensureProc() (*agentProc, error) {
	s.mu.Lock()
	proc := s.proc
	resumable, loadable, closed, fresh := s.resumable, s.loadable, s.closed, s.fresh
	changed := s.sandboxed && s.procPin != execPin(s.exe)
	s.mu.Unlock()
	if closed {
		return nil, errSessionLost
	}
	if fresh {
		return s.startFresh()
	}
	if proc != nil && proc.alive() {
		if !changed {
			return proc, nil
		}
		// The descriptor this turn carries is not the one the process was
		// started with: the session continues in a new process under it.
		if !resumable && !loadable {
			return nil, errSandboxChanged
		}
		s.mu.Lock()
		if s.proc == proc {
			s.proc = nil
			s.killTerminalsLocked()
		}
		s.mu.Unlock()
		proc.stop()
		proc = nil
	}

	if !resumable && !loadable {
		s.markLost()
		return nil, errSessionLost
	}
	s.relaunchFromCurrent()
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
	// Setting up a sandbox launch (the first one of an opencode version warms
	// its config directory) is not bound by the handshake timeout.
	launch, pin, err := s.launch(s.ctx)
	if err != nil {
		return nil, err
	}
	proc, err = startAgentIn(ctx, s.agent, s.agent.spawnArgs(s.spawnModel), s.ws.dir, launch, s, profileEnviron(s.profile, pc)...)
	if err != nil {
		s.markLost()
		return nil, err
	}
	servers, err := s.sessionMCPServers(proc, pc)
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
	s.proc, s.procPin = proc, pin
	if opts := resp.normalizeOptions(); len(opts) > 0 {
		s.options = opts
	}
	s.mu.Unlock()
	go s.watch(proc)
	slog.Info("bnbloc-acp session continued in a new agent process", "agent", s.agent.Name, "session", s.id, "resumed", resumable)
	return proc, nil
}

// relaunchFromCurrent keeps a conversation whose program was removed. A session
// continues with the launch it was created with, and a managed agent's file can
// be replaced or removed while the session is idle (an update, a rollback, a
// removal). When a file that launch names is gone and the agent is registered
// now with another launch, the session continues with that one: the agent keeps
// the conversation in its own store, and a version that cannot read it fails the
// continuation visibly, where a missing file would have lost the session at
// once. Said in the log; a session whose files all exist is never touched.
func (s *session) relaunchFromCurrent() {
	s.mu.Lock()
	launch := s.agent
	s.mu.Unlock()
	if launch.guestOnly() {
		return // its files are in the guest: not ours to look for on the host
	}
	missing := ""
	for _, ref := range launchRefs(launch) {
		if filepath.IsAbs(ref) {
			if _, err := os.Stat(ref); err != nil {
				missing = ref
				break
			}
		}
	}
	if missing == "" {
		return
	}
	// The registration may have changed in the last seconds (an update writes it
	// just before the next message): read it now, not at the next 30 s refresh.
	s.reg.reload()
	cur, ok := s.reg.config().Agent(launch.Name)
	if !ok || (cur.Command == launch.Command && slices.Equal(cur.Args, launch.Args) && maps.Equal(cur.Env, launch.Env)) {
		return
	}
	s.mu.Lock()
	s.agent.Command, s.agent.Args, s.agent.Env = cur.Command, cur.Args, cur.Env
	s.mu.Unlock()
	slog.Warn("bnbloc-acp session continues with the agent's current launch: the file it was created with is gone",
		"agent", launch.Name, "session", s.id, "missing", missing, "was", launch.Command, "now", cur.Command)
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

// steer folds text into the running turn when the agent takes it
// (`_session/steering`, docs/prd/2026-10-25-steer-a-running-turn.md in bnbloc):
// injected, idle (no turn runs; nothing was consumed) or unsupported (the
// agent does not offer steering, or refused; nothing was consumed). It never
// starts, queues or ends a turn of the adapter's.
func (s *session) steer(ctx context.Context, text string) (core.SteerOutcome, error) {
	s.mu.Lock()
	proc, running, acpID := s.proc, s.running, s.acpID
	s.mu.Unlock()
	if !running || proc == nil {
		return core.SteerIdle, nil
	}
	if !proc.init.canSteer() {
		return core.SteerUnsupported, nil
	}
	req := acpSteeringRequest{SessionID: acpID, Prompt: []acpContentBlock{{Type: "text", Text: text}}}
	req.Meta.Steering.IdleBehavior = "promptRequired"
	var resp acpSteeringResponse
	if err := proc.call(ctx, methodSessionSteering, req, &resp); err != nil {
		var rerr *rpcError
		if errors.As(err, &rerr) {
			return core.SteerUnsupported, nil // the agent refused (or does not know the method)
		}
		return "", err
	}
	switch resp.Outcome {
	case "injected":
		return core.SteerInjected, nil
	case "promptRequired":
		return core.SteerIdle, nil
	case "startedNewTurn":
		// The turn ended between the check and the call, and an agent without the
		// opt-in started one of its own: stop it, the caller sends the message
		// as an ordinary input.
		_ = proc.conn.Notify(methodSessionCancel, acpCancelNotification{SessionID: acpID})
		return core.SteerIdle, nil
	}
	return core.SteerUnsupported, nil
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
	if s.isSandboxed() {
		s.sbHost.removeToken(s.id)
	}
}

// launch is how this session's agent process is started: confined for a
// sandboxed session (its pin returned too), nil for a host session. It writes
// the current exec token where vmexec reads it.
func (s *session) launch(ctx context.Context) (*sandboxLaunch, string, error) {
	s.mu.Lock()
	sandboxed, exe := s.sandboxed, s.exe
	s.mu.Unlock()
	if !sandboxed {
		return nil, "", nil
	}
	l, err := s.sbHost.prepareLaunch(ctx, s.agent, exe, s.id)
	if err != nil {
		return nil, "", err
	}
	return l, execPin(exe), nil
}

// setExecution takes the descriptor an input carried: the session is sandboxed
// from now on and the token file holds the new token at once (a running turn
// benefits as much as the next one). What a process was started with changes
// only at the next turn's start (ensureProc).
func (s *session) setExecution(exe core.SandboxExecution) error {
	s.mu.Lock()
	if s.closed {
		// A session closed meanwhile (its workspace deleted) must not get its
		// token file made again.
		s.mu.Unlock()
		return errSessionLost
	}
	if _, err := s.sbHost.writeToken(s.id, exe.ExecToken); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("the exec token file: %w", err)
	}
	if !s.sandboxed {
		// Once, before any turn: a running turn reads these without the lock.
		s.pol = policyFor(s.agent.Permissions, true)
		s.ws.confined, s.ws.denyWrite = true, append([]string(nil), exe.HostConfine.DenyWrite...)
	}
	s.sandboxed, s.exe = true, exe
	s.mu.Unlock()
	return nil
}

// isSandboxed reports whether the session runs in the agent sandbox.
func (s *session) isSandboxed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxed
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
	if s.running && !s.startedAt.IsZero() {
		st.StartedAtMs = s.startedAt.UnixMilli()
		st.Activity = append([]turnActivity(nil), s.activity...)
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
		s.noteActivity(activityText, "", "")
		if u.MessageID != "" && s.textID != "" && u.MessageID != s.textID {
			s.finalizeText()
		}
		if u.MessageID != "" {
			s.textID = u.MessageID
		}
		s.text.WriteString(u.Content.Text)
	case "agent_thought_chunk":
		if u.Content != nil && u.Content.Type == "text" {
			s.noteActivity(activityThinking, "", "")
			s.finalizeText()
			s.thinking.WriteString(u.Content.Text)
		}
	case "tool_call", "tool_call_update", "plan":
		// The agent moved on: the text so far is a complete message.
		s.finalizeText()
		if u.SessionUpdate != "plan" {
			s.noteActivity(activityTool, toolTitle(u), u.ToolCallID)
		}
	case "usage_update":
		if u.Used > 0 && u.Size > 0 {
			s.ctxUsed, s.ctxSize = u.Used, u.Size
		}
		if u.Cost != nil {
			s.cost = &costState{amount: u.Cost.Amount, currency: u.Cost.Currency}
		}
	case "session_info_update":
		cause, failed := metaTurnState(u.Meta)
		if cause != "" {
			s.metaLastErr = cause
		}
		if failed {
			s.metaErr = firstNonEmpty(s.metaLastErr, "the agent stopped with a system error")
		}
	}
}

// maxMetaErr bounds the agent error a failed turn reports.
const maxMetaErr = 500

// metaTurnState reads a turn failure an agent reports in session_info_update
// metadata rather than as a JSON-RPC error or an error stop reason. Codex
// (codex-acp) does so: on a provider error it reports the cause in
// _meta.codex.error updates ("Reconnecting... 1/5", additionalDetails the
// provider's answer), then sets _meta.codex.threadStatus.type "systemError",
// streams the error as message text and ends the turn with end_turn. cause
// is an error the update reports (recovered ones too), failed whether it
// marks the turn failed: only the system error status does.
func metaTurnState(meta json.RawMessage) (cause string, failed bool) {
	if len(meta) == 0 {
		return "", false
	}
	var m struct {
		Codex *struct {
			ThreadStatus *struct {
				Type string `json:"type"`
			} `json:"threadStatus"`
			Error *struct {
				Message           string `json:"message"`
				AdditionalDetails string `json:"additionalDetails"`
			} `json:"error"`
		} `json:"codex"`
	}
	if json.Unmarshal(meta, &m) != nil || m.Codex == nil {
		return "", false
	}
	if e := m.Codex.Error; e != nil {
		cause = strings.TrimSpace(firstNonEmpty(e.AdditionalDetails, e.Message))
		if r := []rune(cause); len(r) > maxMetaErr {
			cause = string(r[:maxMetaErr]) + "…"
		}
	}
	return cause, m.Codex.ThreadStatus != nil && m.Codex.ThreadStatus.Type == "systemError"
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
		if s.isSandboxed() {
			// The adapter is not in the sandbox: a command it ran for the agent would
			// run on the host, unconfined and unrouted. The agent runs commands with
			// its own tools, which the confinement covers and the routing sends to the
			// space (the terminal capability is not offered; a rogue agent asks anyway).
			s.noteRefusal("running " + "a command through the adapter")
			return nil, denied("running commands through the adapter in a sandboxed session")
		}
		var req acpCreateTerminalRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(err)
		}
		return s.createTerminal(req)
	case methodTerminalOutput, methodTerminalWaitExit, methodTerminalKill, methodTerminalRelease:
		if s.isSandboxed() {
			return nil, denied("terminals in a sandboxed session")
		}
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
