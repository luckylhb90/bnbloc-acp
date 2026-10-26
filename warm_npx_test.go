package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNpxSpecIsThePackageOfAPlainNpxLaunch(t *testing.T) {
	for _, c := range []struct {
		a    AgentConfig
		want string
	}{
		{AgentConfig{Command: "npx", Args: []string{"-y", "@agentclientprotocol/claude-agent-acp"}}, "@agentclientprotocol/claude-agent-acp"},
		{AgentConfig{Command: "/x/bin/npx", Args: []string{"--yes", "-q", "pkg@1.2.3", "--acp"}}, "pkg@1.2.3"},
		{AgentConfig{Command: "npx.cmd", Args: []string{"pkg"}}, "pkg"},
		{AgentConfig{Command: "npx", Args: []string{"-y", "--registry", "https://r", "pkg"}}, ""}, // a form it does not know is left as it was
		{AgentConfig{Command: "npx", Args: []string{"-p", "a", "b"}}, ""},
		{AgentConfig{Command: "npx", Args: []string{"-y"}}, ""},
		{AgentConfig{Command: "pnpx", Args: []string{"pkg"}}, ""},
		{AgentConfig{Command: "opencode", Args: []string{"acp"}}, ""},
	} {
		if got := c.a.npxSpec(); got != c.want {
			t.Errorf("%v %v: npxSpec = %q, want %q", c.a.Command, c.a.Args, got, c.want)
		}
	}
}

// The entry name is npm's: these two are the directories npx made for the claude
// agent's package, with and without a version tag.
func TestNpxHashIsNpms(t *testing.T) {
	for spec, want := range map[string]string{
		"@agentclientprotocol/claude-agent-acp":        "d820eb7d96bc2600",
		"@agentclientprotocol/claude-agent-acp@latest": "b555b4fead8494dc",
	} {
		if got := npxHash(spec); got != want {
			t.Errorf("npxHash(%q) = %s, want %s", spec, got, want)
		}
	}
}

// fakeNpxEnv puts a fake `npx` first on PATH. Each run appends to calls.log,
// then does what script says (shell, with $E the cache entry of the package).
func fakeNpxEnv(t *testing.T, spec, script string) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls.log")
	body := `#!/bin/sh
echo run >> "` + calls + `"
E="$npm_config_cache/_npx/` + npxHash(spec) + `"
` + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "npx"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func countCalls(t *testing.T, calls string) int {
	t.Helper()
	raw, _ := os.ReadFile(calls)
	return strings.Count(string(raw), "run\n")
}

func npxLaunch(t *testing.T) (*sandboxLaunch, string) {
	t.Helper()
	vmexec, _ := fakeVMExec(t)
	return &sandboxLaunch{vmexec: vmexec, dir: t.TempDir(), home: t.TempDir()}, t.TempDir()
}

const installOK = `mkdir -p "$E/node_modules/.bin" && echo x > "$E/node_modules/.bin/agent"`

func TestWarmNpxInstallsOnceAndTrustsItsMarker(t *testing.T) {
	needSandboxHere(t)
	const spec = "pkg"
	calls := fakeNpxEnv(t, spec, installOK)
	l, npmDir := npxLaunch(t)
	for i := 0; i < 2; i++ {
		if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
			t.Fatal(err)
		}
	}
	if n := countCalls(t, calls); n != 1 {
		t.Errorf("npx ran %d times for two warm-ups; the marker should make the second free", n)
	}
	if _, err := os.Stat(filepath.Join(npmDir, "_npx", npxHash(spec), npxMarker)); err != nil {
		t.Errorf("no marker after the install: %v", err)
	}
}

// An entry the step did not finish is replaced, whatever it holds: a half install
// looks installed to npm and never heals.
func TestWarmNpxReplacesAnEntryWithoutTheMarker(t *testing.T) {
	needSandboxHere(t)
	const spec = "pkg"
	fakeNpxEnv(t, spec, installOK)
	l, npmDir := npxLaunch(t)
	entry := filepath.Join(npmDir, "_npx", npxHash(spec))
	if err := os.MkdirAll(filepath.Join(entry, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(entry, "node_modules", "pkg", "half-written-binary")
	if err := os.WriteFile(junk, []byte("half"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(junk); err == nil {
		t.Error("the unfinished entry's files were kept")
	}
}

// The host's install of the package seeds the space's cache: the install step
// starts from a copy of it, and the copy is the space's own (not the host's directory).
func TestWarmNpxSeedsFromTheHostCache(t *testing.T) {
	needSandboxHere(t)
	const spec = "pkg"
	fakeNpxEnv(t, spec, `true`)
	host := t.TempDir()
	t.Setenv("npm_config_cache", host)
	hostEntry := filepath.Join(host, "_npx", npxHash(spec))
	if err := os.MkdirAll(filepath.Join(hostEntry, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"node_modules/.bin/agent", "node_modules/seeded.txt"} {
		if err := os.WriteFile(filepath.Join(hostEntry, f), []byte("host"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, npmDir := npxLaunch(t)
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(npmDir, "_npx", npxHash(spec))
	if raw, err := os.ReadFile(filepath.Join(entry, "node_modules", "seeded.txt")); err != nil || string(raw) != "host" {
		t.Fatalf("the seed is not in the space's entry: %q, %v", raw, err)
	}
	if err := os.WriteFile(filepath.Join(entry, "node_modules", "seeded.txt"), []byte("space"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(hostEntry, "node_modules", "seeded.txt")); string(raw) != "host" {
		t.Errorf("a write in the space's entry reached the host's: %q", raw)
	}
}

func TestWarmNpxFailureLeavesNoEntryAndRefuses(t *testing.T) {
	needSandboxHere(t)
	const spec = "pkg"
	fakeNpxEnv(t, spec, `mkdir -p "$E/node_modules/pkg"; echo half > "$E/node_modules/pkg/bin"; echo "npm ERR! network" >&2; exit 1`)
	l, npmDir := npxLaunch(t)
	err := warmNpx(context.Background(), l, nil, npmDir, spec)
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("err = %v, want the install's own message", err)
	}
	if _, err := os.Stat(filepath.Join(npmDir, "_npx", npxHash(spec))); err == nil {
		t.Error("a failed install left its entry for the next start to trust")
	}
}

// An agent that keeps running after the install (it ignores end-of-file) does not
// hold the step: the install is done once its executables are linked and it is quiet.
func TestWarmNpxEndsAnAgentThatKeepsRunning(t *testing.T) {
	needSandboxHere(t)
	old := npxQuiet
	npxQuiet = time.Second
	t.Cleanup(func() { npxQuiet = old })
	const spec = "pkg"
	fakeNpxEnv(t, spec, installOK+"; sleep 60")
	l, npmDir := npxLaunch(t)
	start := time.Now()
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("the step waited %s for an agent that was not going to exit", time.Since(start))
	}
}

// No progress for the stall limit ends the install (a hung download), and says so.
func TestWarmNpxEndsAStalledInstall(t *testing.T) {
	needSandboxHere(t)
	old := npxStall
	npxStall = 2 * time.Second
	t.Cleanup(func() { npxStall = old })
	const spec = "pkg"
	fakeNpxEnv(t, spec, `sleep 60`)
	l, npmDir := npxLaunch(t)
	err := warmNpx(context.Background(), l, nil, npmDir, spec)
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("err = %v", err)
	}
}

// At start the entries the install step did not finish are removed (each is
// replaced by a seeded one at its next start) and the finished ones are kept.
func TestSweepRemovesOnlyUnfinishedNpxEntries(t *testing.T) {
	state := t.TempDir()
	mk := func(space, hash string, finished bool) string {
		e := filepath.Join(state, "npm-"+space, "_npx", hash)
		if err := os.MkdirAll(filepath.Join(e, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
		if finished {
			if err := os.WriteFile(filepath.Join(e, npxMarker), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return e
	}
	whole, legacy, half := mk("a", "1111", true), mk("b", "2222", false), mk("c", "3333", false)
	sweepUntrustedNpx(state)
	for e, want := range map[string]bool{whole: true, legacy: false, half: false} {
		if _, err := os.Stat(e); (err == nil) != want {
			t.Errorf("%s: kept=%v, want %v", e, err == nil, want)
		}
	}
}

// An agent that failed to start from an entry makes it untrusted: the next start
// replaces it. A launch with no npx entry (an agent not run through npx) is left alone.
func TestDistrustRemovesTheMarker(t *testing.T) {
	entry := t.TempDir()
	marker := filepath.Join(entry, npxMarker)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	(&sandboxLaunch{}).distrust()
	var none *sandboxLaunch
	none.distrust()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a launch without an entry removed a marker: %v", err)
	}
	(&sandboxLaunch{npxEntry: entry}).distrust()
	if _, err := os.Stat(marker); err == nil {
		t.Error("the entry is still trusted")
	}
}
