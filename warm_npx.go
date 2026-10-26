package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// An agent launched through npx runs from an npm cache of its own in a sandboxed
// session (sandbox.go): one per sandbox space, so a package a prompt-injected
// agent plants there is run by that space alone. Its first start installs the
// package, a few hundred megabytes, and the handshake's clock (handshakeTimeout)
// is running: an install cut off by it leaves a package npm takes for installed
// (a platform binary half written), and every later start of the space fails
// the same way, for good.
//
// So the install is a step of its own, before the start, with no time limit but
// a stall one (no progress for npxStall), and an entry is trusted only once
// that step finished: a marker is written after it. An entry without the marker
// is replaced, whatever state it is in. The entry is seeded from the host's own
// npm cache when that has the package (a copy-on-write clone where the file
// system has them: seconds, no extra disk, and still a copy of the space's
// own), so the download is paid once per machine, not once per room.

// npxMarker is the file in a cache entry that says the install step finished.
const npxMarker = ".bnbloc-installed"

var (
	// npxStall is how long the install may make no progress (no file or byte
	// changed in the cache) before it is ended.
	npxStall = 90 * time.Second
	// npxQuiet is how long a finished install (its executables linked) may sit
	// quiet while the agent it started keeps running, before the step ends it.
	npxQuiet = 5 * time.Second
)

// npxSpec is the package an npx-launched agent runs ("" = the agent is not
// launched by npx in a form this step knows: only -y/--yes/-q/--quiet and one
// package, so an exotic launch is left as it was).
func (a AgentConfig) npxSpec() string {
	if strings.ToLower(strings.TrimSuffix(filepath.Base(a.Command), ".cmd")) != "npx" {
		return ""
	}
	for _, w := range a.Args {
		switch {
		case w == "-y" || w == "--yes" || w == "-q" || w == "--quiet":
		case strings.HasPrefix(w, "-"):
			return ""
		default:
			return w
		}
	}
	return ""
}

// npxHash is the name of an npx cache entry for one package spec: npm hashes
// the sorted specs (here one) with sha512 and keeps 16 hex digits.
func npxHash(spec string) string {
	sum := sha512.Sum512([]byte(spec))
	return hex.EncodeToString(sum[:])[:16]
}

// hostNpmCache is the adapter's own npm cache directory.
func hostNpmCache() string {
	if v := strings.TrimSpace(os.Getenv("npm_config_cache")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".npm")
}

// warmNpx makes sure the space's npm cache holds a finished install of spec.
func warmNpx(ctx context.Context, l *sandboxLaunch, env []string, npmDir, spec string) error {
	entry := filepath.Join(npmDir, "_npx", npxHash(spec))
	if _, err := os.Stat(filepath.Join(entry, npxMarker)); err == nil {
		return nil
	}
	return warmFlight(ctx, entry, "the agent's package "+spec, 0, func(run context.Context) error {
		if _, err := os.Stat(filepath.Join(entry, npxMarker)); err == nil {
			return nil // another request finished it
		}
		return installNpx(run, l, env, npmDir, entry, spec)
	})
}

// installNpx replaces the entry with a seeded or fresh install of spec.
func installNpx(ctx context.Context, l *sandboxLaunch, env []string, npmDir, entry, spec string) error {
	_ = os.RemoveAll(entry)
	if host := hostNpmCache(); host != "" {
		cloneNpxEntry(filepath.Join(host, "_npx", npxHash(spec)), entry)
	}
	argv := append([]string{"confine"}, l.flags...)
	argv = append(argv, "--", "npx", "-y", spec)
	cmd := exec.Command(l.vmexec, argv...)
	cmd.Dir = l.dir
	cmd.WaitDelay = 5 * time.Second
	// The adapter's environment (HOME, PATH: confine and npm need them), then the
	// descriptor's and the routing's. No stdin: the agent that npx runs after the
	// install reads end-of-file and exits.
	cmd.Env = append(append(adapterEnviron(), env...), "npm_config_cache="+npmDir)
	ownProcessGroup(cmd)
	out := &outputTail{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(entry)
		return fmt.Errorf("install the agent's package %s: %w", spec, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() {
		killTree(cmd, true)
		<-exited
	}
	fail := func(why string) error {
		_ = os.RemoveAll(entry)
		return fmt.Errorf("install the agent's package %s: %s: %s", spec, why, tailText(out.String(), 400))
	}
	done := func() error {
		if err := os.WriteFile(filepath.Join(entry, npxMarker), nil, 0o644); err != nil {
			return fail(err.Error())
		}
		return nil
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last, lastChange := progress(npmDir), time.Now()
	for {
		select {
		case err := <-exited:
			if err == nil || linked(entry) {
				return done()
			}
			return fail(err.Error())
		case <-tick.C:
			if p := progress(npmDir); p != last {
				last, lastChange = p, time.Now()
			}
			quiet := time.Since(lastChange)
			switch {
			case linked(entry) && quiet >= npxQuiet:
				stop()
				return done()
			case quiet >= npxStall:
				stop()
				return fail(fmt.Sprintf("no progress for %s", npxStall))
			}
		case <-ctx.Done():
			stop()
			return fail("cancelled")
		}
	}
}

// linked reports that the install reached its last step: the package's
// executables are linked (npm links them after every package is in place).
func linked(entry string) bool {
	bins, err := os.ReadDir(filepath.Join(entry, "node_modules", ".bin"))
	return err == nil && len(bins) > 0
}

// progress is a fingerprint of the cache's size and number of files: it changes
// while an install writes.
func progress(dir string) [2]int64 {
	var files, bytes int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	return [2]int64{files, bytes}
}

// cloneNpxEntry copies the host's install of the package into the space's cache
// (a copy-on-write clone where the file system has them, else a plain copy),
// through a temporary name so a half copy is never the entry. It is best effort:
// without a seed the install downloads the package.
func cloneNpxEntry(from, to string) {
	if st, err := os.Stat(from); err != nil || !st.IsDir() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return
	}
	tmp := to + ".seed"
	_ = os.RemoveAll(tmp)
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"-c", "-R", from, tmp}
	default:
		args = []string{"-R", "--reflink=auto", from, tmp}
	}
	if err := exec.Command("cp", args...).Run(); err != nil {
		if err := exec.Command("cp", "-R", from, tmp).Run(); err != nil {
			_ = os.RemoveAll(tmp)
			return
		}
	}
	// A seed is the host's install, not a finished one of the space's: no marker.
	_ = os.Remove(filepath.Join(tmp, npxMarker))
	if err := os.Rename(tmp, to); err != nil {
		_ = os.RemoveAll(tmp)
	}
}
