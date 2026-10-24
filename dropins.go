package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// dropInDir is the directory, beside the config file, where another
// component puts the agents it manages: <home>/agents.d/*.json, each file the
// same document as agents.json (only its agents and its denyRead are read). The member's own
// agents.json always wins on a name, so a hand-written entry is never
// overridden; among drop-ins the file name order decides, and a name taken is
// reported, never merged. Writers own their own file and nothing else
// (bnbloc docs/prd/2026-10-24-agent-manager.md §2.5).
const dropInDir = "agents.d"

// dropCache keeps the last valid copy of each drop-in file, so a half-written
// or broken one never removes agents that worked a moment ago.
type dropCache struct {
	mu   sync.Mutex
	good map[string]dropDoc
}

// dropDoc is what one drop-in file gives: its agents and its denyRead.
type dropDoc struct {
	agents   []AgentConfig
	denyRead []string
}

func dropInPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), dropInDir)
}

// dropInFiles lists the drop-in files by name.
func dropInFiles(configPath string) []string {
	entries, _ := os.ReadDir(dropInPath(configPath))
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// configStamp identifies the config as a whole: agents.json and every
// drop-in, by name, modification time and size. It changes when any of them
// does, which is what makes the registry reload.
func configStamp(configPath string) string {
	var b strings.Builder
	add := func(name, path string) {
		if st, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "%s|%d|%d;", name, st.ModTime().UnixNano(), st.Size())
		}
	}
	add("agents.json", configPath)
	for _, n := range dropInFiles(configPath) {
		add(dropInDir+"/"+n, filepath.Join(dropInPath(configPath), n))
	}
	return b.String()
}

// loadAll reads agents.json and merges the drop-ins under it. An invalid
// agents.json is an error (the caller keeps the previous config, as before); an
// invalid drop-in is skipped and logged, its last valid copy kept.
func loadAll(path string, cache *dropCache) (*Config, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	taken := map[string]string{}
	for _, a := range cfg.Agents {
		taken[a.Name] = "agents.json"
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.good == nil {
		cache.good = map[string]dropDoc{}
	}
	files := dropInFiles(path)
	present := map[string]bool{}
	for _, name := range files {
		present[name] = true
		doc, err := readDropIn(filepath.Join(dropInPath(path), name))
		if err != nil {
			slog.Warn("bnbloc-acp drop-in invalid; keeping its last valid copy", "file", dropInDir+"/"+name, "error", err)
			cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s/%s is invalid and was not read: %v", dropInDir, name, err))
			doc = cache.good[name]
		} else {
			cache.good[name] = doc
		}
		cfg.DenyRead = unionPaths(cfg.DenyRead, doc.denyRead)
		for _, a := range doc.agents {
			if by, dup := taken[a.Name]; dup {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf("agent %q in %s/%s is not used: %s already defines it", a.Name, dropInDir, name, by))
				continue
			}
			taken[a.Name] = dropInDir + "/" + name
			cfg.Agents = append(cfg.Agents, a)
		}
	}
	for name := range cache.good {
		if !present[name] {
			delete(cache.good, name) // a removed file removes its agents
		}
	}
	// What a component asks to keep from every confined agent applies to every
	// agent of this core, the member's own entries included.
	if len(cfg.DenyRead) > 0 {
		for i := range cfg.Agents {
			cfg.Agents[i].DenyRead = unionPaths(cfg.Agents[i].DenyRead, cfg.DenyRead)
		}
	}
	return cfg, nil
}

// readDropIn parses one drop-in file with the same rules as agents.json.
func readDropIn(path string) (dropDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return dropDoc{}, err
	}
	c, err := ParseConfig(raw)
	if err != nil {
		return dropDoc{}, err
	}
	return dropDoc{agents: c.Agents, denyRead: c.DenyRead}, nil
}
