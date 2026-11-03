package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testKind is a prepared kind whose build writes out.txt naming its key's
// "v", and is ready when out.txt names it.
func testKind() preparedKind {
	return preparedKind{
		Name:  "test",
		Owned: []string{"out.txt"},
		Ready: func(dir string, key map[string]string) bool {
			raw, err := os.ReadFile(filepath.Join(dir, "out.txt"))
			return err == nil && string(raw) == key["v"]
		},
	}
}

func buildWriting(dir, v string, calls *int) func(context.Context) error {
	return func(context.Context) error {
		*calls++
		return os.WriteFile(filepath.Join(dir, "out.txt"), []byte(v), 0o644)
	}
}

// Built once per key; a new key (the program upgraded) rebuilds, and what the
// kind does not own (the core's own files) stays.
func TestEnsurePreparedRebuildsOnlyWhenTheKeyChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.js"), []byte("core's"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for i := 0; i < 2; i++ {
		if err := ensurePrepared(context.Background(), dir, testKind(), map[string]string{"v": "1"}, "test state", 0, buildWriting(dir, "1", &calls)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("built %d times for one key", calls)
	}
	if err := ensurePrepared(context.Background(), dir, testKind(), map[string]string{"v": "2"}, "test state", 0, buildWriting(dir, "2", &calls)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !preparedCurrent(dir, testKind(), map[string]string{"v": "2"}) {
		t.Fatalf("after a new key: %d builds, current=%v", calls, preparedCurrent(dir, testKind(), map[string]string{"v": "2"}))
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "plugin.js")); string(raw) != "core's" {
		t.Fatal("a rebuild removed a file the kind does not own")
	}
}

// A directory from before stamps is rebuilt once (P1), even when it looks fine.
func TestEnsurePreparedRebuildsAnUnstampedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := ensurePrepared(context.Background(), dir, testKind(), map[string]string{"v": "1"}, "test state", 0, buildWriting(dir, "1", &calls)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("an unstamped directory was trusted (%d builds)", calls)
	}
}

// A build whose result is not usable is a failure, not a stamp; a second start
// for the same key reports it without building (P2); a new key, or a repair,
// builds again.
func TestEnsurePreparedHoldsAFailedKey(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	wrong := func(context.Context) error {
		calls++
		return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("old"), 0o644)
	}
	key := map[string]string{"v": "2"}
	err := ensurePrepared(context.Background(), dir, testKind(), key, "test state", 0, wrong)
	if err == nil || !strings.Contains(err.Error(), "not usable for v 2") {
		t.Fatalf("first failure = %v", err)
	}
	err = ensurePrepared(context.Background(), dir, testKind(), key, "test state", 0, wrong)
	if !errors.Is(err, errPreparedFailed) || calls != 1 {
		t.Fatalf("second start = %v after %d builds, want the held failure without a build", err, calls)
	}
	if !strings.Contains(err.Error(), "check --repair") {
		t.Fatalf("the held failure does not say how to retry: %v", err)
	}
	if err := ensurePrepared(context.Background(), dir, testKind(), map[string]string{"v": "3"}, "test state", 0, buildWriting(dir, "3", &calls)); err != nil {
		t.Fatalf("a new key was held too: %v", err)
	}
	if err := rebuildPrepared(context.Background(), dir, testKind(), map[string]string{"v": "3"}, "test state", buildWriting(dir, "3", &calls)); err != nil || !preparedCurrent(dir, testKind(), map[string]string{"v": "3"}) {
		t.Fatalf("repair = %v", err)
	}
}

// A start that fails from prepared state distrusts it: the next start rebuilds,
// and that is not a held failure.
func TestDistrustedPreparedStateRebuilds(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	key := map[string]string{"v": "1"}
	if err := ensurePrepared(context.Background(), dir, testKind(), key, "test state", 0, buildWriting(dir, "1", &calls)); err != nil {
		t.Fatal(err)
	}
	distrustPrepared(dir)
	if err := ensurePrepared(context.Background(), dir, testKind(), key, "test state", 0, buildWriting(dir, "1", &calls)); err != nil || calls != 2 {
		t.Fatalf("after distrust: %v, %d builds", err, calls)
	}
}

// A space's npm cache unused for its expiry goes at start (P4); a used one stays,
// whichever of its entries was used.
func TestExpirePreparedRemovesUnusedNpmSpaces(t *testing.T) {
	state := t.TempDir()
	now := time.Now()
	mk := func(space string, dirAge, entryUse time.Duration) string {
		d := filepath.Join(state, "npm-"+space)
		e := filepath.Join(d, "_npx", "1111")
		if err := os.MkdirAll(e, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeStamp(e, stamp{Kind: npxKind.Name, Key: map[string]string{"package": "p"}, UsedAtMs: now.Add(-entryUse).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-dirAge)
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
		return d
	}
	gone := mk("a", 40*24*time.Hour, 40*24*time.Hour)
	recent := mk("b", 40*24*time.Hour, time.Hour)
	touched := mk("c", time.Hour, 40*24*time.Hour)
	expirePrepared(state, preparedKinds, now)
	for d, want := range map[string]bool{gone: false, recent: true, touched: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s: kept=%v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

// Every prepared kind has a test that builds it for one version and uses it
// with the next (docs/design-review.md lens 11): the test's name carries the
// kind's name, and it says "Upgrade".
func TestEveryPreparedKindHasAnUpgradeTest(t *testing.T) {
	files, _ := filepath.Glob("*_test.go")
	var src strings.Builder
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		src.Write(raw)
	}
	names := regexp.MustCompile(`func (Test\w*Upgrade\w*)\(`).FindAllStringSubmatch(src.String(), -1)
	for _, k := range preparedKinds {
		if k.Ready == nil { // a kind that is never built (only expired) has no upgrade
			continue
		}
		want := strings.ReplaceAll(strings.Title(strings.ReplaceAll(k.Name, "-", " ")), " ", "") //nolint:staticcheck // ASCII names
		found := false
		for _, n := range names {
			if strings.Contains(n[1], want) {
				found = true
			}
		}
		if !found {
			t.Errorf("prepared kind %q has no upgrade test (want a Test…%s…Upgrade… function)", k.Name, want)
		}
	}
}

// The npx entry for a package installed at one version and asked for after the
// package moved on: a start that fails from it distrusts it, and the next start
// installs again (the entry's key is the spec, so a working entry is kept).
func TestNpxPackageEntryAfterAnUpgrade(t *testing.T) {
	needSandboxHere(t)
	const spec = "pkg"
	calls := fakeNpxEnv(t, spec, installOK+` && mkdir -p "$E/node_modules/pkg" && echo '{"version":"'$PKG_VERSION'"}' > "$E/node_modules/pkg/package.json"`)
	l, npmDir := npxLaunch(t)
	entry := filepath.Join(npmDir, "_npx", npxHash(spec))
	t.Setenv("PKG_VERSION", "1.0.0")
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PKG_VERSION", "2.0.0")
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(t, calls); n != 1 || npxInstalled(entry, spec) != "1.0.0" {
		t.Fatalf("a working entry was not kept: %d installs, version %q", n, npxInstalled(entry, spec))
	}
	(&sandboxLaunch{npxEntry: entry}).distrust() // the agent failed to start from it
	if err := warmNpx(context.Background(), l, nil, npmDir, spec); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(t, calls); n != 2 || npxInstalled(entry, spec) != "2.0.0" {
		t.Fatalf("after a failed start: %d installs, version %q", n, npxInstalled(entry, spec))
	}
}

// check lists each prepared directory with what it is built for, and --repair
// clears a held failure and removes an unused space (P3).
func TestCheckListsAndRepairsPreparedState(t *testing.T) {
	state := t.TempDir()
	now := time.Now()
	oc := filepath.Join(state, "opencode-aaaa")
	failed := filepath.Join(state, "opencode-bbbb")
	for d, s := range map[string]stamp{
		oc:     {Kind: opencodeKind.Name, Key: map[string]string{"opencode": "1.19.5", "plugin": "ab"}, UsedAtMs: now.UnixMilli()},
		failed: {Kind: opencodeKind.Name, FailedKey: map[string]string{"opencode": "1.20.0", "plugin": "ab"}, Failure: "no network"},
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeStamp(d, s); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(state, "npm-gone")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	long := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := checkPrepared(state, false, now, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"built", "opencode 1.19.5", "failed for opencode 1.20.0", "no network", "unused (removed at the next start)", "npm-gone"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check does not say %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("check without --repair removed state")
	}
	out.Reset()
	if err := checkPrepared(state, true, now, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("--repair kept an unused space")
	}
	if s, ok := readStamp(failed); ok && s.FailedKey != nil {
		t.Error("--repair kept a held failure")
	}
	if !preparedCurrent(oc, opencodeKind, map[string]string{"opencode": "1.19.5", "plugin": "ab"}) {
		t.Error("--repair cleared a built directory")
	}
}
