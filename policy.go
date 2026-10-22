package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luckylhb90/bnbloc/core"
)

// policy answers an agent's requests of the client. Drives are unattended,
// so nothing is asked of a person: a tool kind is approved when the agent's
// permissions.allow lists it and every path involved lies inside the session
// workspace. See docs/prd/2026-10-11-acp-core.md "Permission policy".
type policy struct {
	all   bool
	allow map[string]bool
}

func newPolicy(p Permissions) policy { return policyFor(p, false) }

// policyFor is the request policy of a session. A sandboxed session approves
// command execution when the operator listed no kinds: the sandbox is what makes
// it safe (the agent's commands are routed into the space), and an agent that
// cannot run commands is not what a member turned the sandbox on for. An
// operator's own list is respected either way.
func policyFor(p Permissions, sandboxed bool) policy {
	kinds := p.Allow
	if kinds == nil {
		kinds = defaultAllowKinds
		if sandboxed {
			kinds = append(append([]string(nil), kinds...), "execute")
		}
	}
	out := policy{allow: map[string]bool{}}
	for _, k := range kinds {
		if k == "*" {
			out.all = true
		}
		out.allow[k] = true
	}
	return out
}

func (p policy) allows(kind string) bool {
	if kind == "" {
		kind = "other"
	}
	return p.all || p.allow[kind]
}

// decidePermission answers session/request_permission: approve when the
// kind is allowed and every named location is inside the workspace. The
// chosen option is a one-time one (allow_once / reject_once) so every call is
// decided again; with no option of the needed kind the answer is cancelled.
func (p policy) decidePermission(ws workspaceRoot, req acpRequestPermissionRequest) (acpPermissionOutcome, string) {
	approve, why := p.allows(req.ToolCall.Kind), ""
	if !approve {
		why = fmt.Sprintf("tool kind %q is not allowed", req.ToolCall.Kind)
	}
	for _, loc := range req.ToolCall.Locations {
		if !approve {
			break
		}
		path, err := ws.agentPath(loc.Path)
		if err == nil {
			if writeKinds[req.ToolCall.Kind] {
				_, err = ws.resolveWritable(path)
			} else {
				_, err = ws.resolve(path)
			}
		}
		if err != nil {
			approve, why = false, err.Error()
		}
	}
	prefer := []string{"reject_once", "reject_always"}
	if approve {
		prefer = []string{"allow_once", "allow_always"}
	}
	for _, kind := range prefer {
		for _, o := range req.Options {
			if o.Kind == kind {
				return acpPermissionOutcome{Outcome: "selected", OptionID: o.OptionID}, why
			}
		}
	}
	return acpPermissionOutcome{Outcome: "cancelled"}, why
}

// writeKinds are the tool kinds that change files: their locations must be
// writable (not .room/, not an agent-config path).
var writeKinds = map[string]bool{"edit": true, "delete": true, "move": true}

// workspaceRoot is one session workspace: the directory the agent runs in
// (dir) and its symlink-resolved form (real) every containment check uses.
type workspaceRoot struct {
	dir  string
	real string
	// workdirBacked: the workspace was made in a sandbox workdir the core does
	// not own (workspaces.createAt): deleting it forgets it and removes
	// nothing in the directory. recordBase is where its records live beside the
	// workspaces root ("" = beside dir, as for an owned workspace).
	workdirBacked bool
	recordBase    string
}

func newWorkspaceRoot(dir string) (workspaceRoot, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return workspaceRoot{}, err
	}
	return workspaceRoot{dir: dir, real: real}, nil
}

var errOutsideWorkspace = errors.New("path is outside the session workspace")

// agentPath turns a path the agent names into an absolute one: absolute paths
// as they are, relative ones against the workspace. A path that is neither
// (Windows: rooted "\\x" or "/x" on the current drive, or drive-relative "C:x")
// is refused: the agent would resolve it against its own current drive or
// directory, not the workspace, so joining it would approve a path outside
// the workspace.
func (w workspaceRoot) agentPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	if filepath.VolumeName(path) != "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	return filepath.Join(w.dir, path), nil
}

// resolve returns the real path of an absolute path, refusing anything that
// resolves outside the workspace (symlinks followed for the part that
// exists; the rest cannot be a link).
func (w workspaceRoot) resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	path = filepath.Clean(path)
	existing, rest := path, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", errOutsideWorkspace
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	real = filepath.Join(real, rest)
	if !within(w.real, real) {
		return "", errOutsideWorkspace
	}
	return real, nil
}

// resolveWritable is resolve for a write: the bridge's .room/ context record
// and the agent-config files (the session's role: instructions, skills, MCP
// config) are read-only to the agent.
func (w workspaceRoot) resolveWritable(path string) (string, error) {
	real, err := w.resolve(path)
	if err != nil {
		return "", err
	}
	if within(filepath.Join(w.real, core.RoomContextDir), real) {
		return "", errors.New("the .room/ context is read-only")
	}
	if rel, err := filepath.Rel(w.real, real); err == nil && core.BnblocOwnedPath(filepath.ToSlash(rel)) {
		return "", errors.New("the agent configuration files are read-only")
	}
	return real, nil
}

func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
