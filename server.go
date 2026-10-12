package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luckylhb90/bnbloc/core/protocol"
)

// adapterVersion is the adapter's own version in discovery and ACP
// clientInfo.
const adapterVersion = "0.1.0"

// maxBodyBytes bounds a Core Protocol request body (a room-context file is
// the largest).
const maxBodyBytes = 32 << 20

// capabilities are the optional Core Protocol capabilities the adapter
// serves; agentProfile is mapped per agent (profile.go). Not sandboxExec: an ACP agent may run shell commands itself rather
// than through client terminals, so the adapter cannot route every command
// into a sandbox space (docs/prd/2026-10-11-acp-core.md "Sandbox").
var capabilities = []string{
	protocol.CapabilityInterrupt, protocol.CapabilityWriteFile, protocol.CapabilityDeleteWorkspace,
	protocol.CapabilitySessionFiles, protocol.CapabilitySessionFileBytes, protocol.CapabilityAgentProfile,
}

// adapter serves Core Protocol v1 over the configured ACP agents.
type adapter struct {
	name  string
	token string
	ws    *workspaces
	reg   *registry

	mu       sync.Mutex
	sessions map[string]*session
	order    map[string][]string // workspace id -> session ids, oldest first
}

func newAdapter(name, token string, ws *workspaces, reg *registry) *adapter {
	return &adapter{name: name, token: token, ws: ws, reg: reg, sessions: map[string]*session{}, order: map[string][]string{}}
}

// apiError is a Core Protocol error envelope with its HTTP status.
type apiError struct {
	status    int
	code      protocol.ErrorCode
	msg       string
	retryable bool
}

func (e *apiError) Error() string { return e.msg }

func errf(status int, code protocol.ErrorCode, msg string) *apiError {
	return &apiError{status: status, code: code, msg: msg}
}

func (a *adapter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.PathDiscover, a.discover)
	mux.HandleFunc("GET "+protocol.PathHealth, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET "+protocol.PathTargets, a.targets)
	mux.HandleFunc("POST "+protocol.PathWorkspaces, a.createWorkspace)
	mux.HandleFunc("DELETE /core/v1/workspaces/{ws}", a.deleteWorkspace)
	mux.HandleFunc("PUT /core/v1/workspaces/{ws}/files/{path...}", a.writeFile)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/files/{path...}", a.readFile)
	mux.HandleFunc("POST /core/v1/workspaces/{ws}/sessions", a.createSession)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/sessions/latest", a.latestSession)
	mux.HandleFunc("POST /core/v1/workspaces/{ws}/sessions/{sid}/input", a.sendInput)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/sessions/{sid}/idle", a.sessionIdle)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/sessions/{sid}/turn", a.pollTurn)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/sessions/{sid}/messages", a.messages)
	mux.HandleFunc("POST /core/v1/workspaces/{ws}/sessions/{sid}/interrupt", a.interrupt)
	mux.HandleFunc("GET /core/v1/workspaces/{ws}/sessions/{sid}/files", a.listFiles)
	mux.HandleFunc("GET /acp/v1/agents", a.agents)
	mux.HandleFunc("GET /version", serveVersion)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, errf(http.StatusNotFound, protocol.ErrorBadRequest, "no route "+r.Method+" "+r.URL.Path))
	})
	return a.auth(mux)
}

// auth requires the configured bearer on every request.
func (a *adapter) auth(next http.Handler) http.Handler {
	if a.token == "" {
		return next
	}
	want := []byte("Bearer " + a.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeErr(w, errf(http.StatusUnauthorized, protocol.ErrorUnauthorized, "missing or wrong bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *adapter) discover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.Discovery{
		Name: a.name, Version: adapterVersion,
		Protocol:     protocol.ProtocolRange{Min: protocol.SupportedMinVersion, Max: protocol.SupportedMaxVersion},
		Capabilities: capabilities,
		Limits:       protocol.Limits{MaxInputBytes: maxBodyBytes},
	})
}

func (a *adapter) targets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.ListTargetsResponse{Targets: a.reg.targets()})
}

// agents is the adapter's own status route: every configured agent with its
// availability and the reason when unavailable (not part of Core Protocol).
func (a *adapter) agents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": a.reg.statuses()})
}

func (a *adapter) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateWorkspaceRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Workdir != "" {
		writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, "this core does not support sandbox execution (no workdir workspaces)"))
		return
	}
	id, err := a.ws.create()
	if err != nil {
		writeErr(w, internalErr("create workspace", err))
		return
	}
	writeJSON(w, http.StatusOK, protocol.CreateWorkspaceResponse{WorkspaceID: id})
}

func (a *adapter) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("ws")
	if _, err := a.ws.get(id); err != nil {
		writeErr(w, wsNotFound())
		return
	}
	a.mu.Lock()
	var doomed []*session
	for _, sid := range a.order[id] {
		doomed = append(doomed, a.sessions[sid])
		delete(a.sessions, sid)
	}
	delete(a.order, id)
	a.mu.Unlock()
	for _, s := range doomed {
		s.close()
	}
	if err := a.ws.remove(id); err != nil {
		writeErr(w, internalErr("delete workspace", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *adapter) writeFile(w http.ResponseWriter, r *http.Request) {
	ws, err := a.ws.get(r.PathValue("ws"))
	if err != nil {
		writeErr(w, wsNotFound())
		return
	}
	var req protocol.WriteWorkspaceFileRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	content := []byte(req.Content)
	if req.ContentBase64 != "" {
		if content, err = base64.StdEncoding.DecodeString(req.ContentBase64); err != nil {
			writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, "contentBase64: "+err.Error()))
			return
		}
	}
	if err := ws.writeWorkspaceFile(r.PathValue("path"), content); err != nil {
		if errors.Is(err, errBadPath) || errors.Is(err, errOutsideWorkspace) {
			writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, err.Error()))
			return
		}
		writeErr(w, internalErr("write workspace file", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *adapter) readFile(w http.ResponseWriter, r *http.Request) {
	ws, err := a.ws.get(r.PathValue("ws"))
	if err != nil {
		writeErr(w, wsNotFound())
		return
	}
	raw, ctype, err := ws.readFile(r.PathValue("path"))
	switch {
	case errors.Is(err, errBadPath):
		writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, err.Error()))
	case errors.Is(err, errFileNotFound):
		writeErr(w, errf(http.StatusNotFound, protocol.ErrorFileNotFound, err.Error()))
	case errors.Is(err, errFileTooLarge):
		writeErr(w, errf(http.StatusRequestEntityTooLarge, protocol.ErrorFileTooLarge, err.Error()))
	case err != nil:
		writeErr(w, internalErr("read workspace file", err))
	default:
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}
}

func (a *adapter) createSession(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("ws")
	ws, err := a.ws.get(wsID)
	if err != nil {
		writeErr(w, wsNotFound())
		return
	}
	var req protocol.CreateSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Execution != nil {
		writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, "this core does not support sandbox execution"))
		return
	}
	prof, err := req.Profile.Core()
	if err != nil {
		writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, err.Error()))
		return
	}
	agent, aerr := a.pickAgent(req.TargetID)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	s := newSession(newID("s-"), wsID, ws, agent, a.reg)
	s.profile = prof
	if err := s.open(r.Context(), req.Prompt, req.Options); err != nil {
		slog.Warn("bnbloc-acp session start failed", "agent", agent.Name, "error", err)
		if errors.Is(err, errSessionOption) || errors.Is(err, errMcpClash) || errors.Is(err, errBadPath) || errors.Is(err, errOutsideWorkspace) {
			writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, agent.Name+": "+err.Error()))
			return
		}
		writeErr(w, errf(http.StatusServiceUnavailable, protocol.ErrorCoreUnavailable, agent.Name+": "+err.Error()))
		return
	}
	a.mu.Lock()
	a.sessions[s.id] = s
	a.order[wsID] = append(a.order[wsID], s.id)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, protocol.CreateSessionResponse{SessionID: s.id, Phase: "running"})
}

// pickAgent resolves a target id to its agent: the named one, or the first
// available agent when none is named.
func (a *adapter) pickAgent(target string) (AgentConfig, *apiError) {
	cfg := a.reg.config()
	if target == "" {
		for _, t := range a.reg.targets() {
			if agent, ok := cfg.Agent(t.ID); ok {
				return agent, nil
			}
		}
		return AgentConfig{}, errf(http.StatusNotFound, protocol.ErrorTargetNotFound, "no ACP agent is available")
	}
	agent, ok := cfg.Agent(target)
	if !ok {
		return AgentConfig{}, errf(http.StatusNotFound, protocol.ErrorTargetNotFound, "no ACP agent is configured as "+strconv.Quote(target))
	}
	return agent, nil
}

func (a *adapter) latestSession(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("ws")
	if _, err := a.ws.get(wsID); err != nil {
		writeErr(w, wsNotFound())
		return
	}
	a.mu.Lock()
	ids := a.order[wsID]
	latest := ""
	if len(ids) > 0 {
		latest = ids[len(ids)-1]
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, protocol.LatestSessionResponse{SessionID: latest})
}

func (a *adapter) session(r *http.Request) (*session, *apiError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[r.PathValue("sid")]
	if s == nil || s.workspaceID != r.PathValue("ws") {
		return nil, errf(http.StatusNotFound, protocol.ErrorSessionNotFound, "no such session")
	}
	return s, nil
}

func (a *adapter) sendInput(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	var req protocol.SendInputRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Execution != nil {
		writeErr(w, errf(http.StatusBadRequest, protocol.ErrorBadRequest, "this core does not support sandbox execution"))
		return
	}
	if err := s.enqueue(turnInput{text: req.Text, opts: req.Options}); err != nil {
		writeErr(w, errf(http.StatusNotFound, protocol.ErrorSessionNotFound, err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *adapter) sessionIdle(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	writeJSON(w, http.StatusOK, protocol.SessionIdleResponse{Idle: s.idle()})
}

func (a *adapter) pollTurn(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	st, ok := s.poll()
	if !ok {
		writeErr(w, errf(http.StatusNotFound, protocol.ErrorSessionNotFound, errSessionLost.Error()))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *adapter) messages(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	msgs, latest := s.messages(after)
	writeJSON(w, http.StatusOK, protocol.MessagesResponse{Messages: msgs, LatestVersion: latest})
}

func (a *adapter) interrupt(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	s.interrupt()
	w.WriteHeader(http.StatusNoContent)
}

func (a *adapter) listFiles(w http.ResponseWriter, r *http.Request) {
	s, aerr := a.session(r)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	files, truncated, err := s.ws.listFiles()
	if err != nil {
		writeErr(w, internalErr("list session files", err))
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListSessionFilesResponse{Files: files, Truncated: truncated})
}

// reap stops idle agents until ctx ends (registry's config gives the
// timeout).
func (a *adapter) reap(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.reapOnce(a.reg.config().idle)
	}
}

func (a *adapter) reapOnce(idle time.Duration) {
	if idle <= 0 {
		return
	}
	a.mu.Lock()
	all := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	a.mu.Unlock()
	for _, s := range all {
		s.reapIfIdle(idle)
	}
}

// close stops every agent process.
func (a *adapter) close() {
	a.mu.Lock()
	all := make([]*session, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	a.sessions = map[string]*session{}
	a.order = map[string][]string{}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			s.close()
		}(s)
	}
	wg.Wait()
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func decodeBody(r *http.Request, out any) *apiError {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return errf(http.StatusBadRequest, protocol.ErrorBadRequest, "read body: "+err.Error())
	}
	if len(raw) > maxBodyBytes {
		return errf(http.StatusRequestEntityTooLarge, protocol.ErrorBadRequest, "request body too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errf(http.StatusBadRequest, protocol.ErrorBadRequest, "invalid JSON body: "+err.Error())
	}
	return nil
}

func wsNotFound() *apiError {
	return errf(http.StatusNotFound, protocol.ErrorWorkspaceNotFound, "no such workspace")
}

func internalErr(what string, err error) *apiError {
	slog.Error("bnbloc-acp request failed", "what", what, "error", err)
	return errf(http.StatusInternalServerError, protocol.ErrorInternal, what+" failed")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		e = internalErr("request", err)
	}
	writeJSON(w, e.status, protocol.ErrorEnvelope{Error: protocol.ProtocolError{Code: e.code, Message: e.msg, Retryable: e.retryable}})
}
