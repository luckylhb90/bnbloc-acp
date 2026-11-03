package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpencode puts an `opencode` of version ver first on PATH that behaves as
// the real one does with a config directory: it writes .gitignore, keeps a
// package.json it finds (never rewriting the pin), and installs the pinned SDK
// version into node_modules.
func fakeOpencode(t *testing.T, ver string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo ` + ver + `; exit 0; fi
d="$OPENCODE_CONFIG_DIR"
[ -f "$d/.gitignore" ] || printf 'node_modules\n' > "$d/.gitignore"
[ -f "$d/package.json" ] || printf '{"dependencies":{"@opencode-ai/plugin":"` + ver + `"}}' > "$d/package.json"
pin=$(sed -n 's/.*"@opencode-ai\/plugin":"\([^"]*\)".*/\1/p' "$d/package.json")
mkdir -p "$d/node_modules/@opencode-ai/plugin"
printf '{"version":"%s"}' "$pin" > "$d/node_modules/@opencode-ai/plugin/package.json"
echo opencode/model-a
`
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A routing config directory set up for an earlier opencode is set up again
// for the installed one: opencode keeps the package.json it finds, so the
// warm-up removes opencode's own setup first. The routing plugin stays.
func TestOpencodeWarmAfterAnUpgrade(t *testing.T) {
	needSandboxHere(t) // the routing exists where the sandbox confines; the fakes are shell scripts
	vmexec, _ := fakeVMExec(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin", "bnbloc-vmexec.js"), []byte("routing"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &sandboxLaunch{home: t.TempDir(), vmexec: vmexec, dir: t.TempDir()}

	fakeOpencode(t, "1.18.35")
	if err := warmOpencode(context.Background(), l, nil, dir); err != nil {
		t.Fatalf("first set-up: %v", err)
	}
	fakeOpencode(t, "1.19.5")
	if err := warmOpencode(context.Background(), l, nil, dir); err != nil {
		t.Fatalf("after the upgrade: %v", err)
	}
	if !opencodeConfigWarm(dir, "1.19.5") {
		raw, _ := os.ReadFile(filepath.Join(dir, "package.json"))
		t.Fatalf("the directory is not set up for the installed opencode: %s", raw)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if json.Unmarshal(raw, &pkg) != nil || pkg.Dependencies["@opencode-ai/plugin"] != "1.19.5" {
		t.Fatalf("package.json = %s", raw)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "plugin", "bnbloc-vmexec.js")); !strings.Contains(string(got), "routing") {
		t.Fatal("the routing plugin was removed with opencode's setup")
	}
}
