package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config is the agent list: operator/member data, never a built-in default.
// See docs/prd/2026-10-11-acp-core.md "Configuration".
type Config struct {
	// IdleTimeout is how long an idle session keeps its agent process
	// ("30m" when empty; "0" keeps it until the adapter stops).
	IdleTimeout string        `json:"idleTimeout,omitempty"`
	Agents      []AgentConfig `json:"agents"`

	idle time.Duration
}

// AgentConfig describes one ACP agent: how to spawn it and the policy its
// requests are answered by.
type AgentConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// AuthMethod is an ACP auth method id passed to `authenticate` before
	// sessions are created (a method that works non-interactively).
	AuthMethod string `json:"authMethod,omitempty"`
	// Mode is an ACP session mode id applied to every session.
	Mode string `json:"mode,omitempty"`
	// McpServers is passed to every session verbatim (an ACP McpServer list).
	McpServers json.RawMessage `json:"mcpServers,omitempty"`
	// Models / ModelArgs apply only when the agent reports no model selector:
	// the catalog to offer, and spawn args with {model} replaced by the
	// chosen model.
	Models      []string    `json:"models,omitempty"`
	ModelArgs   []string    `json:"modelArgs,omitempty"`
	Permissions Permissions `json:"permissions,omitempty"`
	// Profile maps a session's agent profile (an expert's instructions,
	// skills, MCP servers) onto this agent; nil = defaultProfileConfig.
	Profile *ProfileConfig `json:"profile,omitempty"`
}

// Permissions is the agent's request policy (policy.go).
type Permissions struct {
	// Allow lists the ACP tool kinds approved inside the workspace; "*"
	// approves every kind. Nil = defaultAllowKinds.
	Allow []string `json:"allow,omitempty"`
}

// defaultAllowKinds approves reading and editing inside the workspace, never
// command execution, network fetches or mode switches.
var defaultAllowKinds = []string{"read", "search", "think", "edit", "delete", "move"}

// toolKinds is the ACP ToolKind set.
var toolKinds = map[string]bool{
	"read": true, "edit": true, "delete": true, "move": true, "search": true,
	"execute": true, "think": true, "fetch": true, "switch_mode": true, "other": true,
}

var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const defaultIdleTimeout = 30 * time.Minute

// ParseConfig decodes and validates a config document.
func ParseConfig(raw []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

// LoadConfig reads the config file. A missing file is an empty config (the
// core runs and lists no targets), not an error.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := &Config{idle: defaultIdleTimeout}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

func (c *Config) validate() error {
	c.idle = defaultIdleTimeout
	if s := strings.TrimSpace(c.IdleTimeout); s != "" {
		if s == "0" {
			c.idle = 0
		} else {
			d, err := time.ParseDuration(s)
			if err != nil || d < 0 {
				return fmt.Errorf("idleTimeout %q: want a duration like 30m, or 0", c.IdleTimeout)
			}
			c.idle = d
		}
	}
	seen := map[string]bool{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if !agentNameRE.MatchString(a.Name) {
			return fmt.Errorf("agents[%d].name %q: lowercase letters, digits, dot, underscore, dash", i, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("agents[%d].name %q: duplicate", i, a.Name)
		}
		seen[a.Name] = true
		if strings.TrimSpace(a.Command) == "" {
			return fmt.Errorf("agent %q: command is required", a.Name)
		}
		if (len(a.Models) == 0) != (len(a.ModelArgs) == 0) {
			return fmt.Errorf("agent %q: models and modelArgs go together (modelArgs applies the chosen model)", a.Name)
		}
		if len(a.ModelArgs) > 0 && !strings.Contains(strings.Join(a.ModelArgs, "\x00"), "{model}") {
			return fmt.Errorf("agent %q: modelArgs must contain {model}", a.Name)
		}
		if len(a.McpServers) > 0 {
			var list []json.RawMessage
			if json.Unmarshal(a.McpServers, &list) != nil {
				return fmt.Errorf("agent %q: mcpServers must be a JSON array", a.Name)
			}
		}
		if a.Profile != nil {
			if err := a.effectiveProfileConfig().validate(); err != nil {
				return fmt.Errorf("agent %q: %w", a.Name, err)
			}
		}
		for _, k := range a.Permissions.Allow {
			if k != "*" && !toolKinds[k] {
				return fmt.Errorf("agent %q: permissions.allow %q is not an ACP tool kind", a.Name, k)
			}
		}
	}
	return nil
}

// Agent returns the named agent's config.
func (c *Config) Agent(name string) (AgentConfig, bool) {
	for _, a := range c.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return AgentConfig{}, false
}

// spawnArgs is the agent's argv tail for one session: its args plus, for a
// config-declared model catalog, modelArgs with the chosen model.
func (a AgentConfig) spawnArgs(model string) []string {
	args := append([]string(nil), a.Args...)
	if model == "" || len(a.ModelArgs) == 0 {
		return args
	}
	for _, m := range a.ModelArgs {
		args = append(args, strings.ReplaceAll(m, "{model}", model))
	}
	return args
}

// mcpServers is the session/new mcpServers value (an empty list by default).
func (a AgentConfig) mcpServers() json.RawMessage {
	if len(a.McpServers) == 0 {
		return json.RawMessage("[]")
	}
	return a.McpServers
}

// environ is the agent's process environment: the adapter's own minus its
// BNBLOC_ACP_* settings (a token must never reach an agent), plus env.
func (a AgentConfig) environ(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BNBLOC_ACP_") {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range a.Env {
		out = append(out, k+"="+v)
	}
	return append(out, extra...)
}
