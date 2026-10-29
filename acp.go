package main

import (
	"encoding/json"
	"strings"
)

// ACP wire types: the subset of the Agent Client Protocol v1 schema
// (github.com/agentclientprotocol/agent-client-protocol, schema/v1) the
// adapter sends or reads. Unknown fields are ignored on decode, as the spec
// asks of receivers.

// acpProtocolVersion is the ACP major version the adapter speaks.
const acpProtocolVersion = 1

// Agent methods (client → agent).
const (
	methodInitialize     = "initialize"
	methodAuthenticate   = "authenticate"
	methodSessionNew     = "session/new"
	methodSessionLoad    = "session/load"
	methodSessionResume  = "session/resume"
	methodSessionPrompt  = "session/prompt"
	methodSessionCancel  = "session/cancel"
	methodSessionSetMode = "session/set_mode"
	// methodSessionSetModel is the model switch of the earlier ACP model state
	// (`models` in the session answer): used for an agent that reports its models
	// that way and has no model config option.
	methodSessionSetModel = "session/set_model"
	methodSessionSetValue = "session/set_config_option"
	// methodSessionSteering is the steering extension (sent only to an agent
	// that advertised it).
	methodSessionSteering = "_session/steering"
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
	// Meta carries the steering extension's advertisement (_meta.steering).
	Meta struct {
		Steering struct {
			Supported bool `json:"supported"`
		} `json:"steering"`
	} `json:"_meta"`
}

// canSteer: the agent takes `_session/steering` (a message folded into its
// running turn; agentclientprotocol/agent-client-protocol#1261).
func (r acpInitializeResponse) canSteer() bool { return r.Meta.Steering.Supported }

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
	// Models is the earlier ACP model state: the models an agent offers and the
	// current one. The ACP schema has both this and the model config option; an
	// agent may report either. normalizeOptions turns it into the option.
	Models *acpModelState `json:"models"`
}

// acpModelState is the earlier ACP session model state.
type acpModelState struct {
	CurrentModelID  string `json:"currentModelId"`
	AvailableModels []struct {
		ModelID string `json:"modelId"`
		Name    string `json:"name"`
	} `json:"availableModels"`
}

// legacyModelOptionID is the id of the config option made from an agent's earlier
// model state; applying it goes to session/set_model (session.go setOption).
const legacyModelOptionID = "model"

// normalizeOptions is the config options of a session answer: the agent's own, and,
// when it has no model option but reports the earlier model state, one made from
// that state, so the rest of the core sees one shape. The option is marked
// (`legacy`) so a value is applied with session/set_model, not set_config_option.
func (r acpSessionResponse) normalizeOptions() []acpConfigOption {
	if _, ok := findOption(r.ConfigOptions, configCategoryModel); ok || r.Models == nil || len(r.Models.AvailableModels) == 0 {
		return r.ConfigOptions
	}
	values := make([]map[string]string, 0, len(r.Models.AvailableModels))
	for _, m := range r.Models.AvailableModels {
		if m.ModelID != "" {
			values = append(values, map[string]string{"value": m.ModelID, "name": firstNonEmpty(m.Name, m.ModelID)})
		}
	}
	if len(values) == 0 {
		return r.ConfigOptions
	}
	opts, _ := json.Marshal(values)
	cur, _ := json.Marshal(r.Models.CurrentModelID)
	return append(append([]acpConfigOption(nil), r.ConfigOptions...), acpConfigOption{
		ID: legacyModelOptionID, Name: "Model", Category: configCategoryModel, Type: "select",
		CurrentValue: cur, Options: opts, legacy: true,
	})
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
	// legacy marks an option made from the earlier model state (never on the wire).
	legacy bool
}

// values flattens a select option's (possibly grouped) value list.
func (o acpConfigOption) values() []string {
	vals, _ := o.valuesAndNames()
	return vals
}

// valuesAndNames is values and, for the entries that carry one, the display name the agent
// gives each value. An entry whose name is empty, or only repeats the value, has no name here:
// a name that says nothing more than the id adds nothing a client would not show anyway.
func (o acpConfigOption) valuesAndNames() ([]string, map[string]string) {
	if o.Type != "select" || len(o.Options) == 0 {
		return nil, nil
	}
	type entry struct {
		Value *string `json:"value"`
		Name  string  `json:"name"`
	}
	var entries []struct {
		entry
		Options []entry `json:"options"`
	}
	if json.Unmarshal(o.Options, &entries) != nil {
		return nil, nil
	}
	var out []string
	var names map[string]string
	add := func(e entry) {
		out = append(out, *e.Value)
		if n := strings.TrimSpace(e.Name); n != "" && n != *e.Value {
			if names == nil {
				names = map[string]string{}
			}
			names[*e.Value] = n
		}
	}
	for _, e := range entries {
		if e.Value != nil {
			add(e.entry)
			continue
		}
		for _, v := range e.Options {
			if v.Value != nil {
				add(v)
			}
		}
	}
	return out, names
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

type acpSetModelRequest struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
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

// acpSteeringRequest is `_session/steering`: the prompt shape, and the opt-in
// that an idle agent hands the message back instead of starting a turn.
type acpSteeringRequest struct {
	SessionID string            `json:"sessionId"`
	Prompt    []acpContentBlock `json:"prompt"`
	Meta      struct {
		Steering struct {
			IdleBehavior string `json:"idleBehavior"`
		} `json:"steering"`
	} `json:"_meta"`
}

// acpSteeringResponse: injected (joined the running turn), promptRequired (no
// turn runs; the message was not consumed) or startedNewTurn (an agent without
// the opt-in started a turn of its own).
type acpSteeringResponse struct {
	Outcome string `json:"outcome"`
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
	SessionUpdate string `json:"sessionUpdate"`
	// Meta is the agent's own metadata (session_info_update carries an
	// agent's turn state there; see metaTurnState).
	Meta      json.RawMessage  `json:"_meta"`
	Content   *acpContentBlock `json:"content"`
	MessageID string           `json:"messageId"`
	// A tool_call / tool_call_update: its id, title, kind and locations.
	ToolCallID    string                `json:"toolCallId"`
	Title         string                `json:"title"`
	Kind          string                `json:"kind"`
	Locations     []acpToolCallLocation `json:"locations"`
	ConfigOptions []acpConfigOption     `json:"configOptions"`
	Cost          *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
	// A usage_update: the tokens the session's context holds now, and the
	// model's context window.
	Used int64 `json:"used"`
	Size int64 `json:"size"`
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
