package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Prepared state (bnbloc docs/prd/2026-11-03-prepared-state-self-check.md):
// a directory the core builds for a program it does not own (an agent's npm
// package, opencode's routing config) and trusts at later starts. Each such
// directory is built through ensurePrepared, the one owner of "is it still
// good", and carries a stamp naming what it was built for. A stamp that names
// something else (opencode upgraded, the sandbox helper moved, the plugin the
// core writes changed) is a rebuild; no stamp is a rebuild (decision P1);
// a failed start from it removes the stamp (distrustPrepared); a rebuild that
// fails again for the same key is reported and not retried until the key
// changes or `bnbloc-acp check --repair` (P2).

// preparedStamp is the file written last into a prepared directory.
const preparedStamp = ".bnbloc-prepared.json"

// preparedUseEvery bounds how often a use is written into the stamp.
const preparedUseEvery = time.Hour

// stamp is a prepared directory's record of what it was built for.
type stamp struct {
	Kind      string            `json:"kind"`
	Key       map[string]string `json:"key"`
	BuiltAtMs int64             `json:"builtAtMs"`
	UsedAtMs  int64             `json:"usedAtMs,omitempty"`
	// FailedKey is the key whose rebuild failed (no Key then): the next start
	// for that same key reports the failure instead of building again (P2).
	FailedKey map[string]string `json:"failedKey,omitempty"`
	Failure   string            `json:"failure,omitempty"`
}

// preparedKind is one kind of prepared state.
type preparedKind struct {
	// Name is the stamp's kind and how check names it.
	Name string
	// Owned are the paths under the directory the kind's build writes, removed
	// before a rebuild ("." = the whole directory, recreated). What the core
	// itself writes beside them (opencode's routing plugin) is not listed.
	Owned []string
	// Ready reports whether a finished build is usable for key: the check of
	// the program's own output, run once after a build.
	Ready func(dir string, key map[string]string) bool
	// ExpireAfter removes a directory of this kind unused that long (0 =
	// never; P4), at start.
	ExpireAfter time.Duration
	// Glob finds the directories of this kind under the core's state dir.
	Glob string
	// LastUse overrides lastUse for a kind without a stamp of its own.
	LastUse func(dir string) time.Time
}

// preparedKinds are every kind of prepared state, the list check, the start's
// sweep and TestEveryPreparedKindHasAnUpgradeTest read.
var preparedKinds = []preparedKind{opencodeKind, npxKind, npmSpaceKind}

// errPreparedFailed is a rebuild for this key that failed before: it waits for
// the key to change or a repair.
var errPreparedFailed = errors.New("the last preparation for this version failed")

// readStamp reads dir's stamp (ok false: none, or unreadable).
func readStamp(dir string) (stamp, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, preparedStamp))
	if err != nil {
		return stamp{}, false
	}
	var s stamp
	if json.Unmarshal(raw, &s) != nil {
		return stamp{}, false
	}
	return s, true
}

func writeStamp(dir string, s stamp) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, preparedStamp), raw, 0o600)
}

// preparedCurrent reports whether dir is built, for kind, for key.
func preparedCurrent(dir string, kind preparedKind, key map[string]string) bool {
	s, ok := readStamp(dir)
	return ok && s.Kind == kind.Name && s.Key != nil && maps.Equal(s.Key, key)
}

// ensurePrepared makes dir a finished build of kind for key, building it when
// it is not: the owned paths are removed, build runs, Ready must hold, then
// the stamp is written. It is the one place a prepared directory is trusted
// or built. what names it in errors. timeout bounds the build (0 = none; the
// build has its own stall limits). A build is one at a time per directory and
// does not belong to the request that asked (warmFlight).
func ensurePrepared(ctx context.Context, dir string, kind preparedKind, key map[string]string, what string, timeout time.Duration, build func(context.Context) error) error {
	if preparedCurrent(dir, kind, key) {
		notePreparedUse(dir)
		return nil
	}
	if s, ok := readStamp(dir); ok && s.Kind == kind.Name && s.FailedKey != nil && maps.Equal(s.FailedKey, key) {
		return fmt.Errorf("%s: %w (%s); it is tried again when %s; `bnbloc-acp check --repair` tries now", what, errPreparedFailed, s.Failure, describeKey(key))
	}
	return warmFlight(ctx, dir, what, timeout, func(run context.Context) error {
		if preparedCurrent(dir, kind, key) {
			return nil // another request finished it
		}
		return rebuildPrepared(run, dir, kind, key, what, build)
	})
}

// rebuildPrepared builds dir for key unconditionally (ensurePrepared, and a
// repair).
func rebuildPrepared(ctx context.Context, dir string, kind preparedKind, key map[string]string, what string, build func(context.Context) error) error {
	start := time.Now()
	prev, had := readStamp(dir)
	_ = os.Remove(filepath.Join(dir, preparedStamp))
	for _, p := range kind.Owned {
		_ = os.RemoveAll(filepath.Join(dir, p))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	fail := func(err error) error {
		// The failure is recorded for this key (P2); a stamp write that fails
		// leaves none, which is a rebuild at the next start.
		_ = os.MkdirAll(dir, 0o700)
		_ = writeStamp(dir, stamp{Kind: kind.Name, FailedKey: key, Failure: oneLine(err.Error(), 300), BuiltAtMs: time.Now().UnixMilli()})
		return err
	}
	if err := build(ctx); err != nil {
		return fail(err)
	}
	if !kind.Ready(dir, key) {
		return fail(fmt.Errorf("%s: the preparation finished but its result is not usable for %s", what, describeKey(key)))
	}
	now := time.Now().UnixMilli()
	if err := writeStamp(dir, stamp{Kind: kind.Name, Key: key, BuiltAtMs: now, UsedAtMs: now}); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	from := "new"
	if had && prev.Key != nil {
		from = describeKey(prev.Key)
	}
	slog.Info("bnbloc-acp prepared", "kind", kind.Name, "dir", dir, "from", from, "for", describeKey(key), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// distrustPrepared makes dir rebuild at its next start: a start from it failed,
// and it may be the cause. It is not a failed build, so it is never held (P2
// holds only a build that failed).
func distrustPrepared(dir string) {
	if dir != "" {
		_ = os.Remove(filepath.Join(dir, preparedStamp))
	}
}

// notePreparedUse records a use at most once per preparedUseEvery (P4 reads it).
func notePreparedUse(dir string) {
	s, ok := readStamp(dir)
	if !ok || s.Key == nil {
		return
	}
	now := time.Now()
	if now.Sub(time.UnixMilli(s.UsedAtMs)) < preparedUseEvery {
		return
	}
	s.UsedAtMs = now.UnixMilli()
	_ = writeStamp(dir, s)
}

// lastUse is when dir was last used: its stamp's, else its build's, else its
// modification time (an unstamped directory from before stamps).
func lastUse(dir string) time.Time {
	if s, ok := readStamp(dir); ok {
		if s.UsedAtMs > 0 {
			return time.UnixMilli(s.UsedAtMs)
		}
		if s.BuiltAtMs > 0 {
			return time.UnixMilli(s.BuiltAtMs)
		}
	}
	if st, err := os.Stat(dir); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// expirePrepared removes, at start, the directories of each kind with an
// expiry that were not used for that long (P4). No session runs at start.
func expirePrepared(stateDir string, kinds []preparedKind, now time.Time) {
	for _, k := range kinds {
		if k.ExpireAfter <= 0 {
			continue
		}
		dirs, _ := filepath.Glob(filepath.Join(stateDir, k.Glob))
		for _, d := range dirs {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				continue
			}
			used := lastUse(d)
			if k.LastUse != nil {
				used = k.LastUse(d)
			}
			if !used.IsZero() && now.Sub(used) >= k.ExpireAfter {
				if err := os.RemoveAll(d); err == nil {
					slog.Info("bnbloc-acp removed unused prepared state", "kind", k.Name, "dir", d, "unusedFor", now.Sub(used).Round(time.Hour))
				}
			}
		}
	}
}

// describeKey is a key as one line, its names sorted.
func describeKey(key map[string]string) string {
	names := slices.Sorted(maps.Keys(key))
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + " " + key[n]
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// preparedState is one prepared directory as check reports it.
type preparedState struct {
	Kind, Dir, State, For string
	Bytes                 int64
	UsedAt                time.Time
}

// listPrepared reads every prepared directory under stateDir. A stamp states
// what the directory was built for; whether that is still current depends on
// facts only a start knows (the descriptor's sandbox helper), so check states
// what is known: built, unfinished, failed, unused.
func listPrepared(stateDir string, now time.Time) []preparedState {
	var out []preparedState
	for _, k := range preparedKinds {
		dirs, _ := filepath.Glob(filepath.Join(stateDir, k.Glob))
		for _, d := range dirs {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				continue
			}
			ps := preparedState{Kind: k.Name, Dir: d, Bytes: progress(d)[1]}
			ps.UsedAt = lastUse(d)
			if k.LastUse != nil {
				ps.UsedAt = k.LastUse(d)
			}
			s, ok := readStamp(d)
			switch {
			case k.Ready == nil:
				ps.State = "kept"
			case !ok:
				ps.State = "unfinished (prepared again at its next start)"
			case s.FailedKey != nil:
				ps.State = "failed for " + describeKey(s.FailedKey) + ": " + s.Failure
			default:
				ps.State, ps.For = "built", describeKey(s.Key)
			}
			if k.ExpireAfter > 0 && !ps.UsedAt.IsZero() && now.Sub(ps.UsedAt) >= k.ExpireAfter {
				ps.State = "unused (removed at the next start)"
			}
			out = append(out, ps)
		}
	}
	return out
}

// checkPrepared prints the prepared state and, with repair, clears what is
// not a finished build (its next start prepares it again) and removes what is
// unused. A built opencode setup the installed opencode no longer matches is
// rebuilt at its next start whatever check says.
func checkPrepared(stateDir string, repair bool, now time.Time, out io.Writer) error {
	states := listPrepared(stateDir, now)
	if len(states) == 0 {
		return nil
	}
	var total int64
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nPREPARED\tSTATE\tFOR\tSIZE\tLAST USED\tDIR")
	for _, s := range states {
		total += s.Bytes
		used := "-"
		if !s.UsedAt.IsZero() {
			used = s.UsedAt.Format("2006-01-02")
		}
		name, _ := filepath.Rel(stateDir, s.Dir)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Kind, s.State, s.For, humanBytes(s.Bytes), used, name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "prepared state: %d directories, %s\n", len(states), humanBytes(total))
	if !repair {
		return nil
	}
	expirePrepared(stateDir, preparedKinds, now)
	sweepUntrustedNpx(stateDir)
	cleared := 0
	for _, s := range states {
		if st, ok := readStamp(s.Dir); ok && st.FailedKey != nil {
			_ = os.Remove(filepath.Join(s.Dir, preparedStamp))
			cleared++
		}
	}
	fmt.Fprintf(out, "repaired: unused and unfinished state removed, %d failed preparation(s) cleared; each is prepared again at its next start\n", cleared)
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
