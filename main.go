// Command bnbloc-acp is the ACP core: a Core Protocol v1 adapter daemon that
// makes any Agent Client Protocol agent (https://agentclientprotocol.com) a
// bnbloc core. It serves Core Protocol v1 on loopback HTTP to the bridge and
// speaks ACP (JSON-RPC 2.0 over stdio) to one agent subprocess per session.
// The agents are configuration (a JSON file), never code: see
// docs/prd/2026-10-11-acp-core.md and
// docs/architecture/core-providers.md "ACP agents".
//
//	bnbloc-acp [serve] [--listen addr] [--home dir] [--config file] [--name core]
//	bnbloc-acp check   [--home dir] [--config file]
//
// It runs as a desktop-supervised sidecar core package (listenEnv
// BNBLOC_ACP_LISTEN, supervisedEnv BNBLOC_ACP_SUPERVISED), or standalone
// with BNBLOC_CORE_<NAME>_URL pointing at it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

const defaultListen = "127.0.0.1:7341"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bnbloc-acp:", err)
		os.Exit(1)
	}
}

type options struct {
	listen, home, config, name string
}

func parseOptions(name string, args []string) (options, error) {
	fs := flag.NewFlagSet("bnbloc-acp "+name, flag.ContinueOnError)
	var o options
	fs.StringVar(&o.listen, "listen", envOr("BNBLOC_ACP_LISTEN", defaultListen), "listen address (host:port)")
	fs.StringVar(&o.home, "home", os.Getenv("BNBLOC_ACP_HOME"), "state directory: agents.json and workspaces/ (default <user config dir>/bnbloc-acp)")
	fs.StringVar(&o.config, "config", os.Getenv("BNBLOC_ACP_CONFIG"), "agent config file (default <home>/agents.json)")
	fs.StringVar(&o.name, "name", "acp", "core name reported in discovery")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if o.home == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return o, fmt.Errorf("no --home and no user config dir: %w", err)
		}
		o.home = filepath.Join(dir, "bnbloc-acp")
	}
	// Absolute: a sandboxed agent is started with paths under it (the confine
	// helper refuses a relative one).
	if abs, err := filepath.Abs(o.home); err == nil {
		o.home = abs
	}
	if o.config == "" {
		o.config = filepath.Join(o.home, "agents.json")
	}
	return o, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		o, err := parseOptions(cmd, args)
		if err != nil {
			return err
		}
		return serve(o)
	case "check":
		o, err := parseOptions(cmd, args)
		if err != nil {
			return err
		}
		return check(o, os.Stdout)
	case "version":
		fmt.Printf("bnbloc-acp %s %s %s\n", version, commit, builtAt)
		return nil
	default:
		return fmt.Errorf("unknown command %q (serve | check | version)", cmd)
	}
}

func serve(o options) error {
	token := os.Getenv("BNBLOC_ACP_TOKEN")
	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", o.listen, err)
	}
	if ip := net.ParseIP(host); token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--listen %q is not loopback: set BNBLOC_ACP_TOKEN to serve beyond this machine", o.listen)
	}
	reg, err := newRegistry(o.config, probeDirFor(o.home))
	if err != nil {
		return err
	}
	ws, err := newWorkspaces(filepath.Join(o.home, "workspaces"))
	if err != nil {
		return err
	}
	a := newAdapter(o.name, token, ws, reg)
	// Sessions live in memory: none outlives the process, so a token file found
	// now belongs to a session that is gone.
	a.sandbox.sweep()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Getenv("BNBLOC_ACP_SUPERVISED") == "1" {
		// The supervisor holds our stdin; it closing means the parent is gone.
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second}
	go reg.run(ctx)
	go a.reap(ctx)
	slog.Info("bnbloc-acp serving Core Protocol v1", "listen", ln.Addr().String(), "config", o.config, "agents", len(reg.config().Agents))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			a.close()
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	a.close()
	return nil
}

// check probes every configured agent once and prints its availability and
// catalog: the member's way to see why an agent is not offered.
func check(o options, out io.Writer) error {
	reg, err := newRegistry(o.config, probeDirFor(o.home))
	if err != nil {
		return err
	}
	cfg := reg.config()
	if len(cfg.Agents) == 0 {
		fmt.Fprintf(out, "no agents configured in %s\n", o.config)
		return nil
	}
	results := make([]agentStatus, len(cfg.Agents))
	var wg sync.WaitGroup
	for i, agent := range cfg.Agents {
		wg.Add(1)
		go func(i int, agent AgentConfig) {
			defer wg.Done()
			results[i] = reg.probe(context.Background(), agent)
		}(i, agent)
	}
	wg.Wait()
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tMODELS\tREASONING\tSANDBOX")
	for i, st := range results {
		routing, note := cfg.Agents[i].sandboxStatus()
		sandbox := "no: " + note
		if routing != "" {
			sandbox = "yes (" + routing + ")"
		}
		if !st.Available {
			fmt.Fprintf(tw, "%s\tunavailable: %s\t\t\t%s\n", st.Name, st.Reason, sandbox)
			continue
		}
		models := describeCatalog(st.Models, st.DefaultModel)
		if !st.ModelSelector && len(cfg.Agents[i].Models) > 0 {
			models = describeCatalog(cfg.Agents[i].Models, "") + " (from config)"
		}
		fmt.Fprintf(tw, "%s\tavailable\t%s\t%s\t%s\n", st.Name, models, describeCatalog(st.ReasoningEfforts, st.DefaultReasoning), sandbox)
	}
	return tw.Flush()
}

func describeCatalog(values []string, current string) string {
	if len(values) == 0 {
		return "-"
	}
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v
		if v == current {
			out[i] = v + "*"
		}
	}
	return strings.Join(out, ", ")
}
