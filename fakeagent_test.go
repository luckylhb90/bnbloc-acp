package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The test binary doubles as a fake ACP agent: with ACP_FAKE_AGENT=1 in its
// environment it speaks ACP on stdio instead of running tests. Prompts are
// commands (see fakeAgent.prompt); env switches select capabilities:
//
//	ACP_FAKE_RESUME=1   sessionCapabilities.resume
//	ACP_FAKE_LOAD=1     loadSession (replays history on load)
//	ACP_FAKE_AUTH=<id>  session/new fails with auth-required until authenticate(<id>)
//	ACP_FAKE_NO_MODELS=1  no config options
//	ACP_FAKE_DIE=1      print to stderr and exit before answering anything
func TestMain(m *testing.M) {
	if os.Getenv("ACP_FAKE_AGENT") == "1" {
		if os.Getenv("ACP_FAKE_DIE") == "1" {
			fmt.Fprintln(os.Stderr, "fatal: local detail /home/maya/.agent/state")
			os.Exit(1)
		}
		(&fakeAgent{out: bufio.NewWriter(os.Stdout), pending: map[int]chan json.RawMessage{}, cancel: map[string]chan struct{}{},
			model: "fake-small", effort: "low", mode: "default"}).serve()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type fakeAgent struct {
	wmu sync.Mutex
	out *bufio.Writer

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan json.RawMessage
	cancel   map[string]chan struct{}
	authed   bool
	session  string
	resumed  bool
	model    string
	effort   string
	mode     string
	costUSD  float64
	turns    int
	sessions int
}

func (f *fakeAgent) serve() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "session/cancel":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			if ch := f.cancel[p.SessionID]; ch != nil {
				close(ch)
				delete(f.cancel, p.SessionID)
			}
			f.mu.Unlock()
		case msg.Method != "":
			go f.request(msg.ID, msg.Method, msg.Params)
		default:
			id, _ := strconv.Atoi(string(msg.ID))
			f.mu.Lock()
			ch := f.pending[id]
			delete(f.pending, id)
			f.mu.Unlock()
			if ch != nil {
				if len(msg.Error) > 0 {
					ch <- json.RawMessage(`{"__error":` + string(msg.Error) + `}`)
				} else {
					ch <- msg.Result
				}
			}
		}
	}
}

func (f *fakeAgent) send(v any) {
	raw, _ := json.Marshal(v)
	f.wmu.Lock()
	defer f.wmu.Unlock()
	f.out.Write(raw)
	f.out.WriteByte('\n')
	f.out.Flush()
}

func (f *fakeAgent) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeAgent) fail(id json.RawMessage, code int, msg string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeAgent) update(u map[string]any) {
	f.mu.Lock()
	sid := f.session
	f.mu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": u}})
}

// call sends a client request and waits for its result (an error comes back
// as {"__error": …}).
func (f *fakeAgent) call(method string, params any) json.RawMessage {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	ch := make(chan json.RawMessage, 1)
	f.pending[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-ch
}

// configOptions is the option list; the caller holds f.mu.
func (f *fakeAgent) configOptions() any {
	if os.Getenv("ACP_FAKE_NO_MODELS") == "1" {
		return nil
	}
	return []map[string]any{
		{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": f.model,
			"options": []map[string]any{{"group": "small", "name": "Small", "options": []map[string]any{{"value": "fake-small", "name": "Small"}}}, {"group": "large", "name": "Large", "options": []map[string]any{{"value": "fake-large", "name": "Large"}}}}},
		{"id": "effort", "name": "Effort", "category": "thought_level", "type": "select", "currentValue": f.effort,
			"options": []map[string]any{{"value": "low", "name": "Low"}, {"value": "high", "name": "High"}}},
	}
}

func (f *fakeAgent) request(id json.RawMessage, method string, params json.RawMessage) {
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	switch method {
	case "initialize":
		caps := map[string]any{"loadSession": os.Getenv("ACP_FAKE_LOAD") == "1", "sessionCapabilities": map[string]any{}}
		if os.Getenv("ACP_FAKE_RESUME") == "1" {
			caps["sessionCapabilities"] = map[string]any{"resume": map[string]any{}}
		}
		f.reply(id, map[string]any{"protocolVersion": 1, "agentCapabilities": caps, "authMethods": []any{}})
	case "authenticate":
		if p["methodId"] != "" && p["methodId"] == os.Getenv("ACP_FAKE_AUTH") {
			f.mu.Lock()
			f.authed = true
			f.mu.Unlock()
			f.reply(id, map[string]any{})
			return
		}
		f.fail(id, -32602, "unknown auth method")
	case "session/new":
		f.mu.Lock()
		if os.Getenv("ACP_FAKE_AUTH") != "" && !f.authed {
			f.mu.Unlock()
			f.fail(id, -32000, "Authentication required")
			return
		}
		f.sessions++
		f.session = fmt.Sprintf("acp-%d-%d", os.Getpid(), f.sessions)
		result := map[string]any{"sessionId": f.session, "configOptions": f.configOptions(),
			"modes": map[string]any{"currentModeId": f.mode, "availableModes": []map[string]any{{"id": "default", "name": "Default"}, {"id": "accept", "name": "Accept"}}}}
		f.mu.Unlock()
		f.reply(id, result)
	case "session/resume", "session/load":
		f.mu.Lock()
		f.session, f.resumed = p["sessionId"].(string), true
		f.mu.Unlock()
		if method == "session/load" {
			f.update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "old prompt"}})
			f.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "REPLAYED"}})
		}
		f.mu.Lock()
		result := map[string]any{"configOptions": f.configOptions()}
		f.mu.Unlock()
		f.reply(id, result)
	case "session/set_config_option":
		v, _ := p["value"].(string)
		f.mu.Lock()
		switch {
		case p["configId"] == "model" && (v == "fake-small" || v == "fake-large"):
			f.model = v
		case p["configId"] == "effort" && (v == "low" || v == "high"):
			f.effort = v
		default:
			f.mu.Unlock()
			f.fail(id, -32602, "invalid value "+v)
			return
		}
		result := map[string]any{"configOptions": f.configOptions()}
		f.mu.Unlock()
		f.reply(id, result)
	case "session/set_mode":
		f.mu.Lock()
		f.mode, _ = p["modeId"].(string)
		f.mu.Unlock()
		f.reply(id, map[string]any{})
	case "session/prompt":
		f.prompt(id, p)
	default:
		f.fail(id, -32601, "method not found: "+method)
	}
}

func (f *fakeAgent) text(s string, msgID string) {
	u := map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": s}}
	if msgID != "" {
		u["messageId"] = msgID
	}
	f.update(u)
}

func (f *fakeAgent) prompt(id json.RawMessage, p map[string]any) {
	var text string
	if blocks, ok := p["prompt"].([]any); ok && len(blocks) > 0 {
		text, _ = blocks[0].(map[string]any)["text"].(string)
	}
	f.mu.Lock()
	f.turns++
	f.costUSD += 0.25
	cost := f.costUSD
	cancel := make(chan struct{})
	f.cancel[f.session] = cancel
	sid := f.session
	whoami := fmt.Sprintf("session=%s resumed=%v model=%s effort=%s mode=%s args=%s", f.session, f.resumed, f.model, f.effort, f.mode, strings.Join(os.Args[1:], " "))
	f.mu.Unlock()
	cmd, arg, _ := strings.Cut(text, " ")
	end := func(reason string) {
		f.update(map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000, "cost": map[string]any{"amount": cost, "currency": "USD"}})
		f.reply(id, map[string]any{"stopReason": reason, "usage": map[string]any{"inputTokens": 3, "outputTokens": 2, "totalTokens": 5}})
	}
	switch cmd {
	case "two":
		f.text("first", "m1")
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "look", "kind": "read", "status": "completed"})
		f.text("second", "m2")
		end("end_turn")
	case "ids":
		f.text("alpha", "a")
		f.text("beta", "b")
		end("end_turn")
	case "think":
		f.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "pondering"}})
		f.text("answer", "")
		end("end_turn")
	case "slow":
		f.text("working", "")
		<-cancel
		f.reply(id, map[string]any{"stopReason": "cancelled"})
	case "fail":
		f.fail(id, -32603, "boom")
	case "refuse":
		f.reply(id, map[string]any{"stopReason": "refusal"})
	case "crash":
		os.Exit(3)
	case "whoami":
		f.text(whoami, "")
		end("end_turn")
	case "permission":
		kind, path, _ := strings.Cut(arg, " ")
		res := f.call("session/request_permission", map[string]any{"sessionId": sid,
			"toolCall": map[string]any{"toolCallId": "t1", "kind": kind, "title": "do " + kind, "locations": locations(path)},
			"options":  []map[string]any{{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, {"optionId": "always", "name": "Always", "kind": "allow_always"}, {"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
		f.text("permission="+compact(res), "")
		end("end_turn")
	case "quiet":
		// Asks to run a command and, refused, ends without a reply.
		f.call("session/request_permission", map[string]any{"sessionId": sid,
			"toolCall": map[string]any{"toolCallId": "t1", "kind": "execute", "title": "rm -rf build"},
			"options":  []map[string]any{{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, {"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
		end("end_turn")
	case "write":
		path, content, _ := strings.Cut(arg, " ")
		f.text("write="+compact(f.call("fs/write_text_file", map[string]any{"sessionId": sid, "path": path, "content": content})), "")
		end("end_turn")
	case "read":
		f.text("read="+compact(f.call("fs/read_text_file", map[string]any{"sessionId": sid, "path": arg, "line": 2, "limit": 1})), "")
		end("end_turn")
	case "terminal":
		res := f.call("terminal/create", map[string]any{"sessionId": sid, "command": arg, "outputByteLimit": 5})
		var created struct {
			TerminalID string `json:"terminalId"`
		}
		if json.Unmarshal(res, &created) != nil || created.TerminalID == "" {
			f.text("terminal="+compact(res), "")
			end("end_turn")
			return
		}
		exit := f.call("terminal/wait_for_exit", map[string]any{"sessionId": sid, "terminalId": created.TerminalID})
		out := f.call("terminal/output", map[string]any{"sessionId": sid, "terminalId": created.TerminalID})
		f.call("terminal/release", map[string]any{"sessionId": sid, "terminalId": created.TerminalID})
		f.text("exit="+compact(exit)+" output="+compact(out), "")
		end("end_turn")
	default:
		f.text("hello ", "m")
		f.text("world", "m")
		end("end_turn")
	}
}

func locations(path string) []map[string]any {
	if path == "" {
		return nil
	}
	return []map[string]any{{"path": path}}
}

func compact(raw json.RawMessage) string {
	return strings.TrimSpace(string(raw))
}
