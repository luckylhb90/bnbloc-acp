package main

import (
	"encoding/json"
)

// ACP wire types: the subset of the Agent Client Protocol v1 schema
// (github.com/agentclientprotocol/agent-client-protocol, schema/v1) the
// adapter sends or reads. Unknown fields are ignored on decode, as the spec
// asks of receivers.

// acpProtocolVersion is the ACP major version the adapter speaks.
const acpProtocolVersion = 1

// Agent methods (client → agent).
const (
	methodInitialize      = "initialize"
	methodAuthenticate    = "authenticate"
	methodSessionNew      = "session/new"
	methodSessionLoad     = "session/load"
	methodSessionResume   = "session/resume"
	methodSessionPrompt   = "session/prompt"
	methodSessionCancel   = "session/cancel"
	methodSessionSetMode  = "session/set_mode"
	methodSessionSetValue = "session/set_config_option"
)

// Client methods (agent → client).
const (
	methodSessionUpdate     = "session/update"
	methodRequestPermission = "session/request_permission"
	methodFSReadTextFile    = "fs/read_text_file"
	methodFSWriteTextFile   = "fs/write_text_file"
	methodTerminalCreate    = "terminal/create"
	methodTerminalOutput    = "terminal/output"
	methodTerminalWaitExit  = "terminal/wait_for_exit"
	methodTerminalKill      = "terminal/kill"
	methodTerminalRelease   = "terminal/release"
	methodCancelRequest     = "$/cancel_request"
)

// Session config option categories the adapter maps.
const (
	configCategoryModel   = "model"
	configCategoryThought = "thought_level"
	configCategoryMode    = "mode"
)

// Prompt stop reasons.
const (
	stopReasonCancelled      = "cancelled"
	stopReasonRefusal        = "refusal"
	stopReasonMaxTokens      = "max_tokens"
	stopReasonMaxTurnRequest = "max_turn_requests"
	// stopReasonError is not one of ACP's stop reasons: some agents end a
	// turn that failed with it instead of a JSON-RPC error.
	stopReasonError = "error"
)

type acpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type acpFSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type acpClientCapabilities struct {
	FS       acpFSCapabilities `json:"fs"`
	Terminal bool              `json:"terminal"`
}

type acpInitializeRequest struct {
	ProtocolVersion    int                   `json:"protocolVersion"`
	ClientCapabilities acpClientCapabilities `json:"clientCapabilities"`
	ClientInfo         acpImplementation     `json:"clientInfo"`
}

type acpInitializeResponse struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession     bool `json:"loadSession"`
		McpCapabilities struct {
			HTTP bool `json:"http"`
			SSE  bool `json:"sse"`
		} `json:"mcpCapabilities"`
		SessionCapabilities struct {
			Resume json.RawMessage `json:"resume"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
	AgentInfo *acpImplementation `json:"agentInfo"`
}

// canResume: the agent supports session/resume (continue without replay).
func (r acpInitializeResponse) canResume() bool {
	raw := r.AgentCapabilities.SessionCapabilities.Resume
	return len(raw) > 0 && string(raw) != "null"
}

type acpAuthenticateRequest struct {
	MethodID string `json:"methodId"`
}

// acpNewSessionRequest is the session/new body. Meta carries the profile's
// system-prompt append for agents mapped with instructionsVia "meta".
type acpNewSessionRequest struct {
	Cwd        string          `json:"cwd"`
	McpServers json.RawMessage `json:"mcpServers"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

// acpContinueSessionRequest is the session/resume and session/load body.
type acpContinueSessionRequest struct {
	SessionID  string          `json:"sessionId"`
	Cwd        string          `json:"cwd"`
	McpServers json.RawMessage `json:"mcpServers"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type acpModeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []struct {
		ID string `json:"id"`
	} `json:"availableModes"`
}

type acpSessionResponse struct {
	SessionID     string            `json:"sessionId"`
	Modes         *acpModeState     `json:"modes"`
	ConfigOptions []acpConfigOption `json:"configOptions"`
}

// acpConfigOption is one session config option. Only "select" options carry
// a value list; a boolean option is kept but never mapped to a catalog.
type acpConfigOption struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Category     string          `json:"category,omitempty"`
	Type         string          `json:"type"`
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      json.RawMessage `json:"options,omitempty"`
}

// values flattens a select option's (possibly grouped) value list.
func (o acpConfigOption) values() []string {
	if o.Type != "select" || len(o.Options) == 0 {
		return nil
	}
	var entries []struct {
		Value   *string `json:"value"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if json.Unmarshal(o.Options, &entries) != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Value != nil {
			out = append(out, *e.Value)
			continue
		}
		for _, v := range e.Options {
			out = append(out, v.Value)
		}
	}
	return out
}

// current is a select option's current value ("" for other types).
func (o acpConfigOption) current() string {
	var s string
	if json.Unmarshal(o.CurrentValue, &s) != nil {
		return ""
	}
	return s
}

type acpSetConfigOptionRequest struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type acpSetConfigOptionResponse struct {
	ConfigOptions []acpConfigOption `json:"configOptions"`
}

type acpSetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type acpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type acpPromptRequest struct {
	SessionID string            `json:"sessionId"`
	Prompt    []acpContentBlock `json:"prompt"`
}

type acpPromptResponse struct {
	StopReason string `json:"stopReason"`
	// Usage is the per-turn token usage some agents report (an unstable ACP
	// field; read when present).
	Usage *struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
}

type acpCancelNotification struct {
	SessionID string `json:"sessionId"`
}

type acpSessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// acpSessionUpdate holds the fields of every session/update variant the
// adapter reads; SessionUpdate names the variant.
type acpSessionUpdate struct {
	SessionUpdate string            `json:"sessionUpdate"`
	Content       *acpContentBlock  `json:"content"`
	MessageID     string            `json:"messageId"`
	ConfigOptions []acpConfigOption `json:"configOptions"`
	Cost          *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
}

type acpToolCallLocation struct {
	Path string `json:"path"`
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type acpRequestPermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string                `json:"toolCallId"`
		Kind       string                `json:"kind"`
		Title      string                `json:"title"`
		Locations  []acpToolCallLocation `json:"locations"`
	} `json:"toolCall"`
	Options []acpPermissionOption `json:"options"`
}

type acpPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type acpRequestPermissionResponse struct {
	Outcome acpPermissionOutcome `json:"outcome"`
}

type acpReadTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	Limit     *int   `json:"limit"`
}

type acpReadTextFileResponse struct {
	Content string `json:"content"`
}

type acpWriteTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type acpEnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type acpCreateTerminalRequest struct {
	SessionID       string           `json:"sessionId"`
	Command         string           `json:"command"`
	Args            []string         `json:"args"`
	Env             []acpEnvVariable `json:"env"`
	Cwd             *string          `json:"cwd"`
	OutputByteLimit *int64           `json:"outputByteLimit"`
}

type acpCreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}

type acpTerminalRequest struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type acpTerminalExitStatus struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}

type acpTerminalOutputResponse struct {
	Output     string                 `json:"output"`
	Truncated  bool                   `json:"truncated"`
	ExitStatus *acpTerminalExitStatus `json:"exitStatus,omitempty"`
}
