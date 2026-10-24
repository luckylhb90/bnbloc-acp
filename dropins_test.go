package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(c *Config) string {
	var n []string
	for _, a := range c.Agents {
		n = append(n, a.Name+"="+a.Command)
	}
	return strings.Join(n, ",")
}

func TestDropInsAreMergedUnderTheMembersOwnConfig(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agents.json")
	writeFile(t, cfgPath, `{"agents":[{"name":"claude","command":"mine"}]}`)
	writeFile(t, filepath.Join(home, dropInDir, "b.json"), `{"agents":[{"name":"claude","command":"b-claude"},{"name":"zed","command":"b-zed"}]}`)
	writeFile(t, filepath.Join(home, dropInDir, "a.json"), `{"agents":[{"name":"gemini","command":"a-gemini"},{"name":"zed","command":"a-zed"}]}`)
	writeFile(t, filepath.Join(home, dropInDir, "ignored.txt"), `not json`)
	writeFile(t, filepath.Join(home, dropInDir, ".hidden.json"), `{"agents":[{"name":"ghost","command":"x"}]}`)

	c, err := loadAll(cfgPath, &dropCache{})
	if err != nil {
		t.Fatal(err)
	}
	// agents.json first and always wins; then drop-ins in file name order,
	// the first to name an agent keeping it.
	if got, want := names(c), "claude=mine,gemini=a-gemini,zed=a-zed"; got != want {
		t.Errorf("merged: %s, want %s", got, want)
	}
	if len(c.Notes) != 2 {
		t.Errorf("each name taken is reported: %v", c.Notes)
	}
}

func TestDropInsWorkWithoutAnAgentsJson(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, dropInDir, "m.json"), `{"agents":[{"name":"pi","command":"pi"}]}`)
	c, err := loadAll(filepath.Join(home, "agents.json"), &dropCache{})
	if err != nil || names(c) != "pi=pi" {
		t.Errorf("%v %v", names(c), err)
	}
}

func TestABrokenDropInKeepsItsLastValidCopyAndNeverTheOthers(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agents.json")
	drop := filepath.Join(home, dropInDir, "m.json")
	other := filepath.Join(home, dropInDir, "o.json")
	writeFile(t, drop, `{"agents":[{"name":"pi","command":"pi"}]}`)
	writeFile(t, other, `{"agents":[{"name":"gemini","command":"gemini"}]}`)
	cache := &dropCache{}
	if c, err := loadAll(cfgPath, cache); err != nil || names(c) != "gemini=gemini,pi=pi" && names(c) != "pi=pi,gemini=gemini" {
		t.Fatalf("%v %v", names(c), err)
	}
	// Half-written, and an entry that fails the agents.json rules.
	for _, body := range []string{`{"agents":[{"name":"pi"`, `{"agents":[{"name":"Bad Name","command":"x"}]}`} {
		writeFile(t, drop, body)
		c, err := loadAll(cfgPath, cache)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Agent("pi"); !ok {
			t.Errorf("a broken drop-in removed an agent that worked: %s", names(c))
		}
		if _, ok := c.Agent("gemini"); !ok {
			t.Error("a broken drop-in took another file's agents with it")
		}
		if len(c.Notes) == 0 {
			t.Error("a broken drop-in is reported")
		}
	}
	// A file that is gone takes its agents with it: that is a removal.
	if err := os.Remove(drop); err != nil {
		t.Fatal(err)
	}
	c, _ := loadAll(cfgPath, cache)
	if _, ok := c.Agent("pi"); ok {
		t.Error("a removed drop-in must remove its agents")
	}
}

func TestAnInvalidAgentsJsonStillKeepsThePreviousConfig(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agents.json")
	writeFile(t, cfgPath, `{"agents":[{"name":"claude","command":"mine"}]}`)
	writeFile(t, filepath.Join(home, dropInDir, "m.json"), `{"agents":[{"name":"pi","command":"pi"}]}`)
	reg, err := newRegistry(cfgPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfgPath, `{oops`)
	reg.reload()
	if got := names(reg.config()); got != "claude=mine,pi=pi" {
		t.Errorf("the member's typo does not take the core or the drop-ins down: %s", got)
	}
}

func TestTheRegistryReloadsWhenADropInAppearsChangesAndGoes(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "agents.json")
	writeFile(t, cfgPath, `{"agents":[{"name":"claude","command":"mine"}]}`)
	reg, err := newRegistry(cfgPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.config().Agents) != 1 {
		t.Fatal("start")
	}
	drop := filepath.Join(home, dropInDir, "agent-manager.json")
	writeFile(t, drop, `{"agents":[{"name":"gemini","command":"/store/gemini","args":["--acp"]}]}`)
	reg.reload()
	a, ok := reg.config().Agent("gemini")
	if !ok || a.Command != "/store/gemini" || a.Args[0] != "--acp" {
		t.Fatalf("a new drop-in is picked up: %+v", a)
	}
	writeFile(t, drop, `{"agents":[{"name":"gemini","command":"/store/gemini-2","args":["--acp"]}]}`)
	// The size changes too, so the stamp changes even within the clock's
	// resolution.
	reg.reload()
	if a, _ = reg.config().Agent("gemini"); a.Command != "/store/gemini-2" {
		t.Errorf("a changed drop-in is picked up: %+v", a)
	}
	if err := os.Remove(drop); err != nil {
		t.Fatal(err)
	}
	reg.reload()
	if _, ok := reg.config().Agent("gemini"); ok {
		t.Error("a removed drop-in is picked up")
	}
}
