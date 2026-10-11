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

// get returns an existing workspace's root.
func (w *workspaces) get(id string) (workspaceRoot, error) {
	if !workspaceIDRE.MatchString(id) {
		return workspaceRoot{}, errWorkspaceNotFound
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
	return os.RemoveAll(ws.dir)
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

// writeRoomFile writes the bridge's room-context file; only the .room/
// subtree is writable this way.
func (ws workspaceRoot) writeRoomFile(rel string, content []byte) error {
	p, err := relPath(rel)
	if err != nil {
		return err
	}
	if !within(roomDir, p) {
		return fmt.Errorf("%w: room context files live under %s/", errBadPath, roomDir)
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
// symlinks), hidden and dependency trees skipped, capped at
// protocol.MaxSessionFiles.
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
		if err != nil {
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
