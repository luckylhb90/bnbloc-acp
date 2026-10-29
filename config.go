package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// DenyRead lists absolute paths no confined run of any agent may read, in
	// agents.json and in each drop-in (the union applies to every agent). It can
	// only restrict: a component that keeps a credential on this machine names
	// it here, whatever agent a member runs.
	DenyRead []string `json:"denyRead,omitempty"`

	// Notes say what was left out of the merged config and why (an agent a
	// drop-in defines that agents.json already does, an invalid drop-in);
	// `check` prints them. Never read from a file.
	Notes []string `json:"-"`

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
	// Models / ModelArgs: the catalog to offer. With ModelArgs, for an agent that
	// reports no model selector: spawn args with {model} replaced by the chosen
	// model. Without ModelArgs, for an agent that reports a selector but only its
	// current model (Hermes: its session answer lists the model in force, and it
	// switches to any of its configured ones): the models it can be switched to,
	// each applied through the agent's own switch (session/set_config_option, or
	// session/set_model for an agent that reports the earlier model state).
	Models    []string `json:"models,omitempty"`
	ModelArgs []string `json:"modelArgs,omitempty"`
	// EnvProvider is a program run for the agent on the host, never confined,
	// whose output is added to the agent's environment (envprovider.go).
	EnvProvider *EnvProviderConfig `json:"envProvider,omitempty"`
	// DenyRead lists absolute paths a confined run of this agent may not read,
	// added to the sandbox descriptor's own. It can only restrict.
	DenyRead []string `json:"denyRead,omitempty"`
	// CleanEnv drops the credentials the adapter's own environment carries (cloud
	// and API keys, tokens, an ssh-agent socket: credentialEnvName) from a
	// sandboxed run of this agent. A confined agent's network is open, so each is
	// one it can send away. What the agent's own `env`, its environment provider
	// and the sandbox descriptor set is kept. False (the usual): the agent
	// inherits the environment, as an agent of the member's own may need to.
	CleanEnv    bool        `json:"cleanEnv,omitempty"`
	Permissions Permissions `json:"permissions,omitempty"`
	// Profile maps a session's agent profile (an expert's instructions,
	// skills, MCP servers) onto this agent; nil = defaultProfileConfig.
	Profile *ProfileConfig `json:"profile,omitempty"`
	// StderrActivity marks steps of a running turn that the agent reports
	// only in its log (stderr), e.g. waiting out a model rate limit
	// (stderr.go).
	StderrActivity []StderrRule `json:"stderrActivity,omitempty"`
	// Sandbox overrides the routing this agent's CLI has for running its shell
	// commands in the agent sandbox's space: "opencode", "claude", "codex", or "off".
	// Empty (the usual): the routing is recognised from the command, as vmexec
	// recognises a harness from the executable (sandbox.go). An agent with none
	// cannot run sandboxed, and a sandboxed request for it is refused.
	Sandbox string `json:"sandbox,omitempty"`
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
	if err := validDenyRead("denyRead", c.DenyRead); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if err := validDenyRead(fmt.Sprintf("agent %q: denyRead", a.Name), a.DenyRead); err != nil {
			return err
		}
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
		if len(a.ModelArgs) > 0 && len(a.Models) == 0 {
			return fmt.Errorf("agent %q: modelArgs apply a model chosen from models: list them", a.Name)
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
		for j := range a.StderrActivity {
			if err := a.StderrActivity[j].compile(); err != nil {
				return fmt.Errorf("agent %q: stderrActivity[%d]: %w", a.Name, j, err)
			}
		}
		if a.EnvProvider != nil && strings.TrimSpace(a.EnvProvider.Command) == "" {
			return fmt.Errorf("agent %q: envProvider.command is required", a.Name)
		}
		if a.guestOnly() {
			if len(a.Env) > 0 || a.EnvProvider != nil {
				return fmt.Errorf("agent %q: a guest agent (sandbox \"guest\") takes no env or envProvider: its environment is the guest's own (the bridge's local egress providers give a space what it needs)", a.Name)
			}
		}
		if !validSandboxRouting(a.Sandbox) {
			return fmt.Errorf("agent %q: sandbox %q is not a routing this core implements (%s, %s, %s, or off)", a.Name, a.Sandbox, sandboxRoutingOpencode, sandboxRoutingClaude, sandboxRoutingCodex)
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
	return a.environFrom(os.Environ(), extra...)
}

// sandboxedEnviron is environ for a confined run: with CleanEnv, the adapter's
// inherited credentials stay out.
func (a AgentConfig) sandboxedEnviron(extra ...string) []string {
	inherited := os.Environ()
	if a.CleanEnv {
		inherited = withoutCredentialEnv(inherited)
	}
	return a.environFrom(inherited, extra...)
}

func (a AgentConfig) environFrom(inherited []string, extra ...string) []string {
	var out []string
	for _, kv := range inherited {
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

// validDenyRead refuses a path that is not absolute (a relative one would mean
// something else for every process that reads it).
func validDenyRead(what string, paths []string) error {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q: want an absolute path", what, p)
		}
	}
	return nil
}

// unionPaths is a followed by the paths of b it does not have, cleaned.
func unionPaths(a []string, b ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range append([][]string{a}, b...) {
		for _, p := range list {
			if c := filepath.Clean(p); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}
