package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/core/protocol"
)

// Probe cadence: an unavailable agent is retried every minute (installed or
// signed in later = available without a restart); an available one's model
// catalog is refreshed every ten minutes (live sessions refresh it too).
const (
	refreshTick      = 30 * time.Second
	retryUnavailable = time.Minute
	refreshAvailable = 10 * time.Minute
	// probeTimeout covers a first npx start, which downloads the agent.
	probeTimeout = 5 * time.Minute
)

// agentStatus is one configured agent's availability and catalog, from its
// last probe (or live session).
type agentStatus struct {
	Name             string   `json:"name"`
	Available        bool     `json:"available"`
	Reason           string   `json:"reason,omitempty"`
	ModelSelector    bool     `json:"modelSelector"`
	Models           []string `json:"models,omitempty"`
	DefaultModel     string   `json:"defaultModel,omitempty"`
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	DefaultReasoning string   `json:"defaultReasoning,omitempty"`
	CheckedAtMs      int64    `json:"checkedAtMs,omitempty"`
}

// registry holds the agent config (re-read when the file changes) and each
// agent's status. Targets are served from it, never probed per request.
type registry struct {
	path     string
	probeDir string

	mu      sync.Mutex
	cfg     *Config
	stamp   fileStamp
	status  map[string]agentStatus
	probing map[string]bool
}

type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool
}

func stampOf(path string) fileStamp {
	st, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mod: st.ModTime(), size: st.Size(), ok: true}
}

func newRegistry(path, probeDir string) (*registry, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return &registry{path: path, probeDir: probeDir, cfg: cfg, stamp: stampOf(path), status: map[string]agentStatus{}, probing: map[string]bool{}}, nil
}

func (r *registry) config() *Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// reload re-reads the config when the file changed. An invalid edit keeps
// the previous config (logged), so a typo never takes the core down.
func (r *registry) reload() {
	st := stampOf(r.path)
	r.mu.Lock()
	same := st == r.stamp
	r.mu.Unlock()
	if same {
		return
	}
	cfg, err := LoadConfig(r.path)
	if err != nil {
		slog.Warn("bnbloc-acp config invalid; keeping the previous one", "path", r.path, "error", err)
		r.mu.Lock()
		r.stamp = st
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stamp = st
	old := r.cfg
	r.cfg = cfg
	for name := range r.status {
		prev, _ := old.Agent(name)
		next, ok := cfg.Agent(name)
		if !ok || !reflect.DeepEqual(prev, next) {
			delete(r.status, name)
		}
	}
	slog.Info("bnbloc-acp config reloaded", "path", r.path, "agents", len(cfg.Agents))
}

// run keeps the statuses fresh until ctx ends.
func (r *registry) run(ctx context.Context) {
	t := time.NewTicker(refreshTick)
	defer t.Stop()
	for {
		r.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refresh reloads the config and probes every agent that is due, in
// parallel, without waiting for them.
func (r *registry) refresh(ctx context.Context) {
	r.reload()
	cfg := r.config()
	now := time.Now()
	for _, a := range cfg.Agents {
		r.mu.Lock()
		st, seen := r.status[a.Name]
		age := now.Sub(time.UnixMilli(st.CheckedAtMs))
		due := !seen || (!st.Available && age >= retryUnavailable) || (st.Available && age >= refreshAvailable)
		if !due || r.probing[a.Name] {
			r.mu.Unlock()
			continue
		}
		r.probing[a.Name] = true
		r.mu.Unlock()
		go func(a AgentConfig) {
			st := r.probe(ctx, a)
			r.mu.Lock()
			delete(r.probing, a.Name)
			prev, seen := r.status[a.Name]
			if cur, ok := r.cfg.Agent(a.Name); ok && reflect.DeepEqual(cur, a) {
				r.status[a.Name] = st
			}
			r.mu.Unlock()
			// Log transitions only: a retry with the same outcome is quiet.
			if seen && prev.Available == st.Available && prev.Reason == st.Reason {
				return
			}
			if st.Available {
				slog.Info("bnbloc-acp agent available", "agent", a.Name, "models", len(st.Models), "reasoning", len(st.ReasoningEfforts))
			} else {
				slog.Warn("bnbloc-acp agent unavailable", "agent", a.Name, "reason", st.Reason)
			}
		}(a)
	}
}

// probe starts the agent, opens a session in a scratch directory to read its
// config options (model and reasoning catalogs), and stops it. No prompt is
// sent.
func (r *registry) probe(ctx context.Context, a AgentConfig) agentStatus {
	st := agentStatus{Name: a.Name, CheckedAtMs: time.Now().UnixMilli()}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := os.MkdirAll(r.probeDir, 0o700); err != nil {
		st.Reason = err.Error()
		return st
	}
	dir, err := os.MkdirTemp(r.probeDir, a.Name+"-")
	if err != nil {
		st.Reason = err.Error()
		return st
	}
	defer os.RemoveAll(dir)
	proc, err := startAgent(ctx, a, a.Args, dir, probeHandler{})
	if err != nil {
		st.Reason = localReason(err)
		return st
	}
	defer proc.stop()
	var resp acpSessionResponse
	if err := proc.call(ctx, methodSessionNew, acpNewSessionRequest{Cwd: dir, McpServers: a.mcpServers()}, &resp); err != nil {
		st.Reason = "session/new: " + localReason(proc.diagnose(err))
		var rerr *rpcError
		if errors.As(err, &rerr) && rerr.Code == rpcAuthRequired {
			st.Reason = "authentication required: sign in to the agent on this machine (or set authMethod): " + rerr.Message
		}
		return st
	}
	st.Available = true
	applyCatalog(&st, resp.ConfigOptions)
	return st
}

// applyCatalog maps an agent's config options onto its catalog: the "model"
// select gives the models and the default, "thought_level" the reasoning
// levels.
func applyCatalog(st *agentStatus, opts []acpConfigOption) {
	st.ModelSelector, st.Models, st.DefaultModel = false, nil, ""
	st.ReasoningEfforts, st.DefaultReasoning = nil, ""
	if o, ok := findOption(opts, configCategoryModel); ok {
		st.ModelSelector, st.Models, st.DefaultModel = true, o.values(), o.current()
	}
	if o, ok := findOption(opts, configCategoryThought); ok {
		st.ReasoningEfforts, st.DefaultReasoning = o.values(), o.current()
	}
}

// observe refreshes an agent's catalog from a live session's session/new
// answer (the agent's defaults, before any choice was applied).
func (r *registry) observe(name string, opts []acpConfigOption) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := agentStatus{Name: name, Available: true, CheckedAtMs: time.Now().UnixMilli()}
	applyCatalog(&st, opts)
	r.status[name] = st
}

// usesConfigModels: the agent reports no model selector (or was not probed
// yet) and its config declares a catalog applied through modelArgs.
func (r *registry) usesConfigModels(a AgentConfig) bool {
	if len(a.Models) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.status[a.Name].ModelSelector
}

// statuses lists every configured agent, unprobed ones as pending.
func (r *registry) statuses() []agentStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]agentStatus, 0, len(r.cfg.Agents))
	for _, a := range r.cfg.Agents {
		st, ok := r.status[a.Name]
		if !ok {
			st = agentStatus{Name: a.Name, Reason: "not checked yet"}
		}
		out = append(out, st)
	}
	return out
}

// targets are the available agents, in config order.
func (r *registry) targets() []protocol.Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []protocol.Target{}
	for _, a := range r.cfg.Agents {
		st, ok := r.status[a.Name]
		if !ok || !st.Available {
			continue
		}
		t := protocol.Target{ID: a.Name, Provider: a.Name, ReasoningEfforts: st.ReasoningEfforts, DefaultReasoning: st.DefaultReasoning}
		if st.ModelSelector {
			t.Models, t.DefaultModel = st.Models, st.DefaultModel
		} else if len(a.Models) > 0 {
			t.Models = a.Models
		}
		out = append(out, t)
	}
	return out
}

// probeHandler answers a probe agent's client requests: nothing is allowed
// (no prompt is ever sent, so none is expected).
type probeHandler struct{}

func (probeHandler) Notify(string, json.RawMessage) {}

func (probeHandler) Request(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
	if method == methodRequestPermission {
		return acpRequestPermissionResponse{Outcome: acpPermissionOutcome{Outcome: "cancelled"}}, nil
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: fmt.Sprintf("%s is not available during a probe", method)}
}

// probeDirFor is the scratch directory probes run in.
func probeDirFor(home string) string { return filepath.Join(home, "probe") }
