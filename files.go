package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/luckylhb90/bnbloc/core"
	"github.com/luckylhb90/bnbloc/core/protocol"
)

// maxReadBytes caps one deliverable read (413 file_too_large above it).
const maxReadBytes = 64 << 20

// listIgnoredDirs are dependency and environment trees a deliverables listing
// skips (hidden entries, .git and the bridge's .room/ included, are skipped
// by name). Web build output (dist, build, out) stays listed: the bridge
// publishes a built site from it.
var listIgnoredDirs = map[string]bool{
	"node_modules": true, "bower_components": true, "vendor": true,
	"venv": true, "__pycache__": true, "target": true,
}

var workspaceIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// workspaces is the directory tree holding one directory per workspace.
type workspaces struct {
	root string
}

var errWorkspaceNotFound = errors.New("workspace not found")

func newWorkspaces(root string) (*workspaces, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &workspaces{root: root}, nil
}

func (w *workspaces) create() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := os.Mkdir(filepath.Join(w.root, id), 0o700); err != nil {
		return "", err
	}
	return id, nil
}

// workdirRecordPath names the file that makes a workspace id stand for a
// directory the core does not own.
func (w *workspaces) workdirRecordPath(id string) string {
	return filepath.Join(w.root, id+".workdir")
}

// createAt makes a workspace of an existing directory (a sandbox workdir the
// bridge created): absolute, a directory, kept as a record beside the
// workspaces root. The core never owns it.
func (w *workspaces) createAt(workdir string) (string, error) {
	if !filepath.IsAbs(workdir) {
		return "", fmt.Errorf("%w: the workdir %q is not absolute", errBadPath, workdir)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(workdir))
	if err != nil {
		return "", fmt.Errorf("%w: the workdir %q: %v", errBadPath, workdir, err)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: the workdir %q is not a directory", errBadPath, workdir)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := os.WriteFile(w.workdirRecordPath(id), []byte(real), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// get returns an existing workspace's root.
func (w *workspaces) get(id string) (workspaceRoot, error) {
	if !workspaceIDRE.MatchString(id) {
		return workspaceRoot{}, errWorkspaceNotFound
	}
	if raw, err := os.ReadFile(w.workdirRecordPath(id)); err == nil {
		dir := strings.TrimSpace(string(raw))
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return workspaceRoot{}, errWorkspaceNotFound
		}
		ws, err := newWorkspaceRoot(dir)
		ws.workdirBacked, ws.recordBase = true, filepath.Join(w.root, id)
		return ws, err
	}
	dir := filepath.Join(w.root, id)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return workspaceRoot{}, errWorkspaceNotFound
	}
	return newWorkspaceRoot(dir)
}

func (w *workspaces) remove(id string) error {
	ws, err := w.get(id)
	if err != nil {
		return err
	}
	if ws.workdirBacked {
		// The directory is the bridge's and the sandbox's: only what the core
		// keeps about it goes.
		if err := os.Remove(w.workdirRecordPath(id)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.RemoveAll(ws.dir); err != nil {
		return err
	}
	if err := os.Remove(ws.profileRecordPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// errBadPath / errFileNotFound / errFileTooLarge map to bad_request,
// file_not_found and file_too_large.
var (
	errBadPath      = errors.New("path must be workspace-relative without ..")
	errFileNotFound = errors.New("no such file in the workspace")
	errFileTooLarge = errors.New("file exceeds the read cap")
)

// relPath validates a workspace-relative slash path from the wire.
func relPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || filepath.IsAbs(p) {
		return "", errBadPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errBadPath
		}
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", errBadPath
	}
	return filepath.FromSlash(clean), nil
}

// writeWorkspaceFile writes a bnbloc-owned workspace file (Core Protocol
// writeFile): the bridge's room context under .room/ or an agent-config
// file (core.WorkspaceFileAllowed, the bnbloc WorkspaceFileWriter contract).
func (ws workspaceRoot) writeWorkspaceFile(rel string, content []byte) error {
	if _, err := relPath(rel); err != nil {
		return err
	}
	if !core.WorkspaceFileAllowed(rel) {
		return fmt.Errorf("%w: workspace files live under %s or the agent-config paths (%s)", errBadPath, core.RoomContextDir, strings.Join(core.BnblocOwnedPaths, ", "))
	}
	return ws.writeAllowed(rel, content)
}

// writeAllowed writes a path already checked against core.WorkspaceFileAllowed,
// refusing one that resolves outside the workspace.
func (ws workspaceRoot) writeAllowed(rel string, content []byte) error {
	p, err := relPath(rel)
	if err != nil {
		return err
	}
	target, err := ws.resolve(filepath.Join(ws.dir, p))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o600)
}

// listFiles lists the workspace's deliverables: regular files only (no
// symlinks), hidden and dependency trees and the agent-config files skipped,
// capped at protocol.MaxSessionFiles.
func (ws workspaceRoot) listFiles() ([]protocol.SessionFile, bool, error) {
	files := []protocol.SessionFile{}
	truncated := false
	err := filepath.WalkDir(ws.real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == ws.real {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") || listIgnoredDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !d.Type().IsRegular() {
			return nil
		}
		if len(files) >= protocol.MaxSessionFiles {
			truncated = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(ws.real, p)
		if err != nil || core.ProtectedWorkspacePath(filepath.ToSlash(rel)) {
			return nil
		}
		files = append(files, protocol.SessionFile{Path: filepath.ToSlash(rel), SizeBytes: info.Size(), ModTimeMs: info.ModTime().UnixMilli()})
		return nil
	})
	return files, truncated, err
}

// readFile returns one regular file's exact bytes and sniffed content type.
func (ws workspaceRoot) readFile(rel string) ([]byte, string, error) {
	p, err := relPath(rel)
	if err != nil {
		return nil, "", err
	}
	target, err := ws.resolve(filepath.Join(ws.dir, p))
	if err != nil {
		return nil, "", errFileNotFound
	}
	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() {
		return nil, "", errFileNotFound
	}
	if st.Size() > maxReadBytes {
		return nil, "", errFileTooLarge
	}
	f, err := os.Open(target)
	if err != nil {
		return nil, "", errFileNotFound
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxReadBytes {
		return nil, "", errFileTooLarge
	}
	return raw, http.DetectContentType(raw), nil
}
