package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigValidates(t *testing.T) {
	cases := map[string]string{
		`{"agents":[{"name":"Bad Name","command":"x"}]}`:                                 "lowercase",
		`{"agents":[{"name":"a","command":"x"},{"name":"a","command":"y"}]}`:             "duplicate",
		`{"agents":[{"name":"a"}]}`:                                                      "command is required",
		`{"agents":[{"name":"a","command":"x","modelArgs":["--model","{model}"]}]}`:      "list them",
		`{"agents":[{"name":"a","command":"x","models":["m"],"modelArgs":["--model"]}]}`: "{model}",
		`{"agents":[{"name":"a","command":"x","permissions":{"allow":["shell"]}}]}`:      "not an ACP tool kind",
		`{"agents":[{"name":"a","command":"x","mcpServers":{}}]}`:                        "JSON array",
		`{"idleTimeout":"soon","agents":[]}`:                                             "idleTimeout",
		`{"agents":[],"extra":true}`:                                                     "unknown field",
	}
	for raw, want := range cases {
		if _, err := ParseConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseConfig(%s) = %v, want %q", raw, err, want)
		}
	}
	c, err := ParseConfig([]byte(`{"idleTimeout":"0","agents":[{"name":"a","command":"x"}]}`))
	if err != nil || c.idle != 0 {
		t.Fatalf("idleTimeout 0 = %v, %v", c, err)
	}
	c, err = ParseConfig([]byte(`{"agents":[]}`))
	if err != nil || c.idle != 30*time.Minute {
		t.Fatalf("default idle = %v, %v", c, err)
	}
}

func TestMissingConfigIsEmpty(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil || len(c.Agents) != 0 || c.idle != defaultIdleTimeout {
		t.Fatalf("LoadConfig(missing) = %+v, %v", c, err)
	}
}

// The shipped example must stay a valid config.
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("examples", "agents.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Agents) == 0 {
		t.Fatal("example lists no agents")
	}
}

func TestSpawnArgsAndPolicyDefaults(t *testing.T) {
	a := AgentConfig{Args: []string{"acp"}, ModelArgs: []string{"--model={model}"}}
	if got := strings.Join(a.spawnArgs("m2"), " "); got != "acp --model=m2" {
		t.Fatalf("spawnArgs = %q", got)
	}
	if got := strings.Join(a.spawnArgs(""), " "); got != "acp" {
		t.Fatalf("spawnArgs without model = %q", got)
	}
	p := newPolicy(Permissions{})
	for _, k := range defaultAllowKinds {
		if !p.allows(k) {
			t.Errorf("default policy refuses %s", k)
		}
	}
	for _, k := range []string{"execute", "fetch", "switch_mode", "other", ""} {
		if p.allows(k) {
			t.Errorf("default policy allows %q", k)
		}
	}
	if !newPolicy(Permissions{Allow: []string{"*"}}).allows("execute") {
		t.Error(`"*" does not allow execute`)
	}
	if newPolicy(Permissions{Allow: []string{}}).allows("read") {
		t.Error("an empty allow list allows read")
	}
}

func TestRegistryReloadsAChangedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"a","command":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(path, probeDirFor(dir))
	if err != nil {
		t.Fatal(err)
	}
	reg.status["a"] = agentStatus{Name: "a", Available: true}
	// An invalid edit keeps the previous config.
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg.reload()
	if len(reg.config().Agents) != 1 || reg.config().Agents[0].Command != "x" {
		t.Fatalf("invalid edit replaced the config: %+v", reg.config())
	}
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"a","command":"y"},{"name":"b","command":"z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(path, future, future)
	reg.reload()
	if len(reg.config().Agents) != 2 {
		t.Fatalf("reload missed the change: %+v", reg.config())
	}
	if _, ok := reg.status["a"]; ok {
		t.Fatal("a changed agent kept its old status")
	}
	if got := reg.statuses(); len(got) != 2 || got[1].Reason != "not checked yet" {
		t.Fatalf("statuses = %+v", got)
	}
}
